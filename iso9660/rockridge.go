package iso9660

import (
	"errors"
	"io"
	"strings"
	"time"
)

// Rock Ridge is a set of annotations tucked into the unused tail of each
// directory record: the long name, the permissions, the symlink target and
// the real timestamps that ISO 9660 has no room for. It is how a disc
// mastered on Unix keeps its filenames, and reading it is the difference
// between offering a user "READMEXX.TXT" and "readme-first.txt".
//
// The annotations are SUSP entries: a two-letter signature, a length, a
// version and a body. When a directory record runs out of room the entries
// continue in a separate area, pointed to by a "CE" entry, so following
// them means reading further off the disc.

const maxContinuations = 8 // a record that chains further than this is broken

// applyRockRidge overlays whatever Rock Ridge says onto a directory record.
// A disc without it is left exactly as it was, which is why this can be
// called unconditionally.
func (f *FS) applyRockRidge(rec *dirRec) {
	if len(rec.system) == 0 {
		return
	}
	var (
		name     strings.Builder
		nameDone bool
		symlink  strings.Builder
	)
	f.walkSUSP(rec.system, func(sig string, body []byte) {
		switch sig {
		case "NM":
			if nameDone || len(body) < 1 {
				return
			}
			// Bit 1 is CURRENT and bit 2 PARENT: those name "." and "..",
			// which are already handled and must not overwrite a real name.
			if body[0]&0x06 != 0 {
				return
			}
			name.Write(body[1:])
			// Bit 0 means the name continues in the next NM entry.
			if body[0]&0x01 == 0 {
				nameDone = true
			}
		case "PX":
			if len(body) >= 4 {
				rec.mode = le32(body[0:4])
			}
		case "SL":
			if len(body) < 1 {
				return
			}
			appendSymlinkComponents(&symlink, body[1:])
		case "TF":
			if t, ok := parseTF(body); ok {
				rec.modTime = t
			}
		}
	})
	if name.Len() > 0 {
		rec.isoName = rec.name
		rec.name = name.String()
	}
	if symlink.Len() > 0 {
		rec.symlink = symlink.String()
	}
}

// walkSUSP calls fn for every entry in a system use area, following "CE"
// continuations into their own sectors. Malformed data ends the walk rather
// than failing: a disc that puts something unexpected here is still a disc
// whose files we can list.
func (f *FS) walkSUSP(area []byte, fn func(sig string, body []byte)) {
	for depth := 0; depth <= maxContinuations; depth++ {
		var next *ceEntry
		for off := 0; off+4 <= len(area); {
			n := int(area[off+2])
			if n < 4 || off+n > len(area) {
				break
			}
			sig := string(area[off : off+2])
			body := area[off+4 : off+n]
			if sig == "CE" {
				if ce, ok := parseCE(body); ok {
					next = &ce
				}
			} else {
				if sig == "ST" { // terminator
					off = len(area)
					break
				}
				fn(sig, body)
			}
			off += n
		}
		if next == nil {
			return
		}
		buf := make([]byte, next.length)
		if _, err := f.r.ReadAt(buf, next.block*BlockSize+next.offset); err != nil && !errors.Is(err, io.EOF) {
			return
		}
		area = buf
	}
}

type ceEntry struct{ block, offset, length int64 }

// parseCE reads a continuation pointer. Each of its three fields is stored
// twice, little-endian then big-endian; the little-endian half is the one
// read here, as everywhere else in this format.
func parseCE(body []byte) (ceEntry, bool) {
	if len(body) < 24 {
		return ceEntry{}, false
	}
	ce := ceEntry{
		block:  int64(le32(body[0:4])),
		offset: int64(le32(body[8:12])),
		length: int64(le32(body[16:20])),
	}
	if ce.length <= 0 || ce.length > 64<<10 {
		return ceEntry{}, false
	}
	return ce, true
}

// appendSymlinkComponents decodes one SL entry's component records onto the
// target being built. A symlink's target is assembled from components so
// that "." and ".." and the root can be recorded without spelling them out.
func appendSymlinkComponents(dst *strings.Builder, body []byte) {
	for off := 0; off+2 <= len(body); {
		flags, n := body[off], int(body[off+1])
		if off+2+n > len(body) {
			return
		}
		text := body[off+2 : off+2+n]
		switch {
		case flags&0x08 != 0: // root
			dst.WriteString("/")
		case flags&0x04 != 0: // parent
			appendComponent(dst, "..")
		case flags&0x02 != 0: // current
			appendComponent(dst, ".")
		default:
			appendComponent(dst, string(text))
		}
		off += 2 + n
	}
}

func appendComponent(dst *strings.Builder, s string) {
	if dst.Len() > 0 && !strings.HasSuffix(dst.String(), "/") {
		dst.WriteString("/")
	}
	dst.WriteString(s)
}

// parseTF returns the modification time, which is the one timestamp worth
// showing. The flags say which of the seven possible times are present and
// in what order, so the wanted one has to be counted up to.
func parseTF(body []byte) (time.Time, bool) {
	if len(body) < 1 {
		return time.Time{}, false
	}
	flags := body[0]
	size := 7
	if flags&0x80 != 0 { // long form: the 17-byte decimal timestamp
		size = 17
	}
	rest := body[1:]
	// Bit 0 is creation, bit 1 modification; only those two can precede the
	// field we want.
	idx := 0
	if flags&0x01 != 0 {
		idx++
	}
	if flags&0x02 == 0 {
		return time.Time{}, false
	}
	off := idx * size
	if off+size > len(rest) {
		return time.Time{}, false
	}
	field := rest[off : off+size]
	if size == 7 {
		t := parseDirTime(field)
		return t, !t.IsZero()
	}
	t := parseVolumeTime(field)
	return t, !t.IsZero()
}

// detectRockRidge looks for the extension record in the root directory's own
// "." entry, which is where a disc declares that it carries Rock Ridge. A
// disc that has the annotations without the declaration is still read - the
// annotations are applied unconditionally - so this only decides what the
// UI reports about the disc.
func (f *FS) detectRockRidge() bool {
	buf := make([]byte, BlockSize)
	if _, err := f.r.ReadAt(buf, f.root.extent*BlockSize); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	n := int(buf[0])
	if n < 34 || n > len(buf) {
		return false
	}
	dot := parseDirRec(buf[:n], false)
	found := false
	f.walkSUSP(dot.system, func(sig string, body []byte) {
		switch sig {
		case "RR":
			found = true
		case "ER":
			// The extension identifier names the extension; Rock Ridge
			// registers itself as either of these two. The body is four
			// lengths - identifier, description, source, extension version
			// - and then the identifier itself.
			if len(body) >= 4 {
				idLen := int(body[0])
				if 4+idLen <= len(body) {
					id := string(body[4 : 4+idLen])
					if strings.HasPrefix(id, "RRIP") || strings.HasPrefix(id, "IEEE_P1282") {
						found = true
					}
				}
			}
		}
	})
	return found
}
