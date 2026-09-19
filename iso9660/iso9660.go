// Package iso9660 reads the filesystem on a data CD or DVD without mounting
// it. Mounting would need root, a free loop device and a kernel that already
// understands the disc; reading the structures directly needs none of those,
// works the same on a disc and on an .iso file, and - the reason it is here -
// lets a single file be pulled off a disc without copying the other 700
// megabytes first.
//
// Three naming schemes coexist on a typical disc and all three are read. The
// ISO 9660 name is the uppercase 8.3 one every reader understands. Joliet
// adds a parallel directory tree with the real, long, Unicode names, which is
// what a disc mastered on Windows carries. Rock Ridge instead annotates the
// original tree with long names, permissions, symlinks and proper
// timestamps, which is what a disc mastered on Unix carries. Joliet is
// preferred when both are present, because its names are what the disc's
// author saw; Rock Ridge fills in everything Joliet has no room for.
package iso9660

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// BlockSize is the logical sector of an ISO 9660 volume. Every structure in
// the format is placed and sized in these.
const BlockSize = 2048

// systemArea is the first 16 sectors, reserved for boot code. The volume
// descriptors start after it.
const systemArea = 16

var (
	// ErrNotISO9660 means the disc has no ISO 9660 volume on it. A disc can
	// be perfectly good and still give this: an audio CD has no filesystem
	// at all, and a UDF-only disc has one this package does not read.
	ErrNotISO9660 = errors.New("no ISO 9660 filesystem on this disc")
	ErrNotFound   = errors.New("no such file or directory on this disc")
	ErrNotDir     = errors.New("not a directory")
	ErrIsDir      = errors.New("is a directory")
)

// Volume is what the disc says about itself. Every field is as recorded,
// trimmed of the padding spaces the format insists on.
type Volume struct {
	VolumeID    string    `json:"volumeId"`
	SystemID    string    `json:"systemId,omitempty"`
	VolumeSetID string    `json:"volumeSetId,omitempty"`
	Publisher   string    `json:"publisher,omitempty"`
	Preparer    string    `json:"preparer,omitempty"`
	Application string    `json:"application,omitempty"`
	Created     time.Time `json:"created,omitzero"`
	Modified    time.Time `json:"modified,omitzero"`
	Sectors     int64     `json:"sectors"`
	Bytes       int64     `json:"bytes"`

	// Which naming schemes this disc carries, which is worth showing: it is
	// the difference between a disc whose names survive a copy and one whose
	// names were flattened to 8.3 by its author.
	Joliet    bool `json:"joliet"`
	RockRidge bool `json:"rockRidge"`
}

// Entry is one file or directory. Path is always absolute within the disc
// and always uses forward slashes, whatever the disc was mastered on.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime,omitzero"`
	// Extent is the first sector of the file's contents. Shown because on a
	// scratched disc it is what says where in the disc a failure was.
	Extent int64 `json:"extent"`
	// Mode and SymlinkTarget come from Rock Ridge, and are empty on a disc
	// that has none.
	Mode          uint32 `json:"mode,omitempty"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
	// ISOName is the plain 8.3 name, kept so a file can still be found by
	// the name a non-Joliet reader would show.
	ISOName string `json:"isoName,omitempty"`
}

// FS is an opened volume. It holds no state beyond the volume descriptor and
// the reader, so several requests may walk it at once.
type FS struct {
	r      io.ReaderAt
	vol    Volume
	root   dirRec
	joliet bool // the active tree is the Joliet one, so names are UCS-2
}

// Open reads the volume descriptors and returns the filesystem. r is read at
// byte offsets; a drive-backed reader that turns those into sector reads is
// in the mmc package.
//
// It reads the descriptors at the start of the volume, which is right for
// an image file and for a disc with one session on it. A disc with more
// than one wants OpenSession.
func Open(r io.ReaderAt) (*FS, error) { return OpenSession(r, 0) }

// OpenSession reads the volume descriptors of the session beginning at
// startSector.
//
// Every session on a multi-session disc carries its own descriptors, and
// only the last set describes the whole disc: a later session's directory
// refers back to the files written earlier as well as to its own. Reading
// the first session's descriptors shows what was on the disc before
// anything was added to it, which is a perfectly plausible answer and the
// wrong one.
//
// The extents those descriptors point at are absolute sector numbers, so
// nothing else in here has to know which session it is reading.
func OpenSession(r io.ReaderAt, startSector int64) (*FS, error) {
	fs := &FS{r: r}
	var primary, supplementary *descriptor

	for i := 0; i < 32; i++ { // a volume with 32 descriptors is already absurd
		buf := make([]byte, BlockSize)
		if _, err := r.ReadAt(buf, (startSector+int64(systemArea+i))*BlockSize); err != nil {
			if i == 0 {
				return nil, fmt.Errorf("reading the volume descriptor: %w", err)
			}
			break
		}
		if string(buf[1:6]) != "CD001" {
			if i == 0 {
				return nil, ErrNotISO9660
			}
			break
		}
		switch buf[0] {
		case 1: // primary
			d := parseDescriptor(buf)
			primary = &d
		case 2: // supplementary; Joliet when its escape sequence says UCS-2
			d := parseDescriptor(buf)
			if isJoliet(buf[88:120]) {
				supplementary = &d
			}
		case 255: // terminator
			i = 32
		}
	}
	if primary == nil {
		return nil, ErrNotISO9660
	}

	active := primary
	if supplementary != nil {
		active = supplementary
		fs.joliet = true
	}
	fs.vol = primary.volume()
	fs.vol.Joliet = supplementary != nil
	if fs.joliet {
		// The Joliet descriptor holds the same identifiers in UCS-2, and
		// those are the ones with the real characters in them.
		v := supplementary.volume()
		mergeIdentifiers(&fs.vol, v)
	}
	fs.root = active.root
	// Rock Ridge announces itself in the system use area of the root
	// directory's own "." record.
	fs.vol.RockRidge = fs.detectRockRidge()
	return fs, nil
}

func (f *FS) Volume() Volume { return f.vol }

// descriptor is the part of a volume descriptor this package uses.
type descriptor struct {
	volumeID    string
	systemID    string
	volumeSetID string
	publisher   string
	preparer    string
	application string
	sectors     int64
	created     time.Time
	modified    time.Time
	root        dirRec
	ucs2        bool
}

func parseDescriptor(b []byte) descriptor {
	d := descriptor{ucs2: b[0] == 2 && isJoliet(b[88:120])}
	str := func(lo, hi int) string { return decodeText(b[lo:hi], d.ucs2) }
	d.systemID = str(8, 40)
	d.volumeID = str(40, 72)
	d.sectors = int64(le32(b[80:84]))
	d.volumeSetID = str(190, 318)
	d.publisher = str(318, 446)
	d.preparer = str(446, 574)
	d.application = str(574, 702)
	d.created = parseVolumeTime(b[813:830])
	d.modified = parseVolumeTime(b[830:847])
	d.root = parseDirRec(b[156:190], d.ucs2)
	return d
}

func (d descriptor) volume() Volume {
	return Volume{
		VolumeID:    d.volumeID,
		SystemID:    d.systemID,
		VolumeSetID: d.volumeSetID,
		Publisher:   d.publisher,
		Preparer:    d.preparer,
		Application: d.application,
		Created:     d.created,
		Modified:    d.modified,
		Sectors:     d.sectors,
		Bytes:       d.sectors * BlockSize,
	}
}

// mergeIdentifiers prefers the Joliet spelling of a name wherever it has
// one, since that is the text with the accents and the punctuation in it.
func mergeIdentifiers(dst *Volume, src Volume) {
	for _, p := range [][2]*string{
		{&dst.VolumeID, &src.VolumeID},
		{&dst.SystemID, &src.SystemID},
		{&dst.VolumeSetID, &src.VolumeSetID},
		{&dst.Publisher, &src.Publisher},
		{&dst.Preparer, &src.Preparer},
		{&dst.Application, &src.Application},
	} {
		if *p[1] != "" {
			*p[0] = *p[1]
		}
	}
}

// isJoliet tests the escape sequences field for the three UCS-2 levels
// Joliet uses. Anything else in a supplementary descriptor is a different
// character set that this package leaves to the primary tree.
func isJoliet(esc []byte) bool {
	s := string(esc)
	return strings.Contains(s, "%/@") || strings.Contains(s, "%/C") || strings.Contains(s, "%/E")
}

// dirRec is one directory record, as it sits on the disc.
type dirRec struct {
	extent  int64
	size    int64
	flags   byte
	name    string
	modTime time.Time
	system  []byte // system use area, where Rock Ridge lives

	// Filled in from Rock Ridge, when the disc carries it.
	isoName string
	mode    uint32
	symlink string
}

func (d dirRec) isDir() bool { return d.flags&0x02 != 0 }

func parseDirRec(b []byte, ucs2 bool) dirRec {
	if len(b) < 33 {
		return dirRec{}
	}
	nameLen := int(b[32])
	d := dirRec{
		extent:  int64(le32(b[2:6])),
		size:    int64(le32(b[10:14])),
		flags:   b[25],
		modTime: parseDirTime(b[18:25]),
	}
	if 33+nameLen > len(b) {
		return d
	}
	d.name = decodeName(b[33:33+nameLen], ucs2)
	// The system use area is whatever is left after the name and its pad
	// byte, and is where Rock Ridge writes.
	start := 33 + nameLen
	if nameLen%2 == 0 {
		start++ // a pad byte keeps the next field even-aligned
	}
	if start < len(b) {
		d.system = b[start:]
	}
	return d
}

// decodeName turns a directory record's name into text. The two special
// names - "\x00" for this directory and "\x01" for its parent - are returned
// as "." and "..", and a version suffix is dropped: nobody wants to see
// README.TXT;1.
func decodeName(b []byte, ucs2 bool) string {
	if len(b) == 1 {
		switch b[0] {
		case 0:
			return "."
		case 1:
			return ".."
		}
	}
	name := decodeText(b, ucs2)
	if i := strings.LastIndexByte(name, ';'); i > 0 {
		name = name[:i]
	}
	// A bare "FOO." is how a name with no extension is written; the trailing
	// dot is padding, not part of the name.
	return strings.TrimSuffix(name, ".")
}

// decodeText handles both the plain and the UCS-2 spelling of a field, and
// trims the trailing spaces the format pads every identifier with.
func decodeText(b []byte, ucs2 bool) string {
	if !ucs2 {
		return strings.TrimRight(string(b), " \x00")
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return strings.TrimRight(string(utf16.Decode(u)), " \x00")
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// parseDirTime reads the 7-byte form used in a directory record: year since
// 1900, month, day, hour, minute, second, and an offset from GMT in quarter
// hours.
func parseDirTime(b []byte) time.Time {
	if len(b) < 7 || (b[0] == 0 && b[1] == 0 && b[2] == 0) {
		return time.Time{}
	}
	zone := time.FixedZone("", int(int8(b[6]))*15*60)
	return time.Date(1900+int(b[0]), time.Month(b[1]), int(b[2]),
		int(b[3]), int(b[4]), int(b[5]), 0, zone)
}

// parseVolumeTime reads the 17-byte decimal form used in a volume
// descriptor: "YYYYMMDDHHMMSShh" and an offset in quarter hours.
func parseVolumeTime(b []byte) time.Time {
	if len(b) < 17 || b[0] == '0' && string(b[:16]) == "0000000000000000" {
		return time.Time{}
	}
	num := func(lo, hi int) int {
		n := 0
		for _, c := range b[lo:hi] {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	year := num(0, 4)
	if year == 0 {
		return time.Time{}
	}
	zone := time.FixedZone("", int(int8(b[16]))*15*60)
	return time.Date(year, time.Month(num(4, 6)), num(6, 8),
		num(8, 10), num(10, 12), num(12, 14), num(14, 16)*10*int(time.Millisecond), zone)
}

// readDir reads every record of one directory extent. A record never
// straddles a sector boundary: a zero length byte means "skip to the next
// sector", which is what the inner loop does.
func (f *FS) readDir(d dirRec) ([]dirRec, error) {
	if !d.isDir() {
		return nil, ErrNotDir
	}
	size := d.size
	if size <= 0 || size > 64<<20 {
		return nil, fmt.Errorf("directory at sector %d has an implausible size of %d bytes", d.extent, size)
	}
	buf := make([]byte, size)
	if _, err := f.r.ReadAt(buf, d.extent*BlockSize); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading the directory at sector %d: %w", d.extent, err)
	}

	var out []dirRec
	for off := 0; off < len(buf); {
		n := int(buf[off])
		if n == 0 {
			// Advance to the start of the next sector.
			next := (off/BlockSize + 1) * BlockSize
			if next <= off {
				break
			}
			off = next
			continue
		}
		if off+n > len(buf) {
			break
		}
		rec := parseDirRec(buf[off:off+n], f.joliet)
		if rec.name != "." && rec.name != ".." {
			f.applyRockRidge(&rec)
			out = append(out, rec)
		}
		off += n
	}
	return out, nil
}

// find walks the tree to p, which must be an absolute, slash-separated path.
func (f *FS) find(p string) (dirRec, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	cur := f.root
	if p == "/" {
		return cur, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		kids, err := f.readDir(cur)
		if err != nil {
			return dirRec{}, err
		}
		found := false
		for _, k := range kids {
			// A disc's names are matched case-insensitively: ISO 9660 names
			// are uppercase by rule, and a user typing a path from a Joliet
			// listing should not have to know which tree they are in.
			if strings.EqualFold(k.name, part) {
				cur, found = k, true
				break
			}
		}
		if !found {
			return dirRec{}, ErrNotFound
		}
	}
	return cur, nil
}

// ReadDir lists one directory. Directories come before files and both are
// sorted by name, so the order is the same every time and matches what a
// file manager would show.
func (f *FS) ReadDir(dir string) ([]Entry, error) {
	rec, err := f.find(dir)
	if err != nil {
		return nil, err
	}
	kids, err := f.readDir(rec)
	if err != nil {
		return nil, err
	}
	base := path.Clean("/" + strings.TrimPrefix(dir, "/"))
	out := make([]Entry, 0, len(kids))
	for _, k := range kids {
		out = append(out, k.entry(path.Join(base, k.name)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Stat describes one path.
func (f *FS) Stat(p string) (Entry, error) {
	rec, err := f.find(p)
	if err != nil {
		return Entry{}, err
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	e := rec.entry(clean)
	if clean == "/" {
		e.Name, e.IsDir = "/", true
	}
	return e, nil
}

func (d dirRec) entry(full string) Entry {
	e := Entry{
		Name:          d.name,
		Path:          full,
		IsDir:         d.isDir(),
		Size:          d.size,
		ModTime:       d.modTime,
		Extent:        d.extent,
		Mode:          d.mode,
		SymlinkTarget: d.symlink,
		ISOName:       d.isoName,
	}
	if e.IsDir {
		// A directory's size is the size of its own records, which is not
		// what anyone means by the size of a folder.
		e.Size = 0
	}
	return e
}

// Open returns the contents of one file. The result reads straight off the
// disc: nothing is buffered up front, so opening a 600 MB file costs
// nothing and a Range request for the middle of it seeks there directly.
func (f *FS) Open(p string) (*io.SectionReader, Entry, error) {
	rec, err := f.find(p)
	if err != nil {
		return nil, Entry{}, err
	}
	if rec.isDir() {
		return nil, Entry{}, ErrIsDir
	}
	e := rec.entry(path.Clean("/" + strings.TrimPrefix(p, "/")))
	return io.NewSectionReader(f.r, rec.extent*BlockSize, rec.size), e, nil
}

// Walk visits every entry in the tree, depth first. It is what the "rip
// everything" and "how big is this disc's content" paths use, and it stops
// at the first error fn returns.
func (f *FS) Walk(root string, fn func(Entry) error) error {
	entries, err := f.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := fn(e); err != nil {
			return err
		}
		if e.IsDir && e.SymlinkTarget == "" {
			if err := f.Walk(e.Path, fn); err != nil {
				return err
			}
		}
	}
	return nil
}
