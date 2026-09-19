// Package udf reads the filesystem on a UDF disc without mounting it.
//
// It exists because a great many discs carry no ISO 9660 at all. Every
// DVD-Video, most game discs and anything written by Windows since XP is
// UDF, and a reader that knows only ISO 9660 says "there is no filesystem
// on this disc" about a disc that is full of files. Some discs are bridged,
// with both filesystems describing the same files; many are not.
//
// What is implemented is reading: the volume, the directory tree, and the
// contents of a file, including a file scattered across the disc in several
// pieces. Writing, named streams, the extended attributes and the virtual
// and sparable partition maps that rewritable packet-written media use are
// not - those belong to a disc being written a block at a time, which is
// not a disc anyone is ripping.
package udf

import (
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Kseen715/ripperX/discfs"
)

// SectorSize is the physical sector of an optical disc. UDF's own block
// size is read from the volume and is this in every case that reaches a
// drive, but the two are separate ideas and are kept separate here.
const SectorSize = 2048

var (
	// ErrNotUDF means the disc has no UDF anchor where one must be. It is
	// the ordinary answer for an audio CD or a plain ISO 9660 disc.
	ErrNotUDF = errors.New("no UDF filesystem on this disc")
	// ErrNotFound and ErrIsDir mirror the ISO 9660 reader's, so the paths
	// above this can treat the two the same way.
	ErrNotFound = errors.New("no such file or directory on this disc")
	ErrIsDir    = errors.New("that is a directory, not a file")
)

// The descriptor tags this reader looks for. A UDF structure announces
// itself by the identifier in its first two bytes.
const (
	tagPrimaryVolume     = 1
	tagAnchorPointer     = 2
	tagPartition         = 5
	tagLogicalVolume     = 6
	tagTerminating       = 8
	tagFileSet           = 256
	tagFileIdentifier    = 257
	tagFileEntry         = 261
	tagExtendedFileEntry = 266
	tagAllocationExtent  = 258
	fileTypeDirectory    = 4
	fileTypeSymlink      = 12
)

// FS is an opened UDF volume.
type FS struct {
	r io.ReaderAt

	blockSize int64
	partStart int64 // first logical block of the partition, from the disc's start
	vol       discfs.Volume
	rootICB   extent
	rootEntry *fileEntry
}

// extent is a run of blocks: where it starts within the partition and how
// many bytes of it are recorded.
type extent struct {
	block  int64
	length int64
	// recorded is false for a hole - space allocated to the file that was
	// never written. It reads back as zeroes, which is what it is.
	recorded bool
}

// Open reads the volume structures. sectors is the length of the disc or
// image in 2048-byte sectors, used to find the anchor at the end when the
// one at 256 is missing; zero means only the usual places are tried.
func Open(r io.ReaderAt, sectors int64) (*FS, error) {
	f := &FS{r: r, blockSize: SectorSize}

	anchor, err := f.findAnchor(sectors)
	if err != nil {
		return nil, err
	}
	// The anchor points at the main volume descriptor sequence: a short run
	// of blocks holding one descriptor each.
	seqStart := int64(le32(anchor[20:24]))
	seqLength := int64(le32(anchor[16:20]))
	if err := f.readVolumeSequence(seqStart, seqLength); err != nil {
		return nil, err
	}
	if f.rootICB.length == 0 {
		return nil, fmt.Errorf("%w: the volume has no root directory", ErrNotUDF)
	}
	root, err := f.readFileEntry(f.rootICB)
	if err != nil {
		return nil, fmt.Errorf("reading the root directory: %w", err)
	}
	f.rootEntry = root
	f.vol.Format = "UDF"
	if f.vol.Sectors == 0 {
		f.vol.Sectors = sectors
	}
	f.vol.Bytes = f.vol.Sectors * SectorSize
	return f, nil
}

// findAnchor looks where the standard says an anchor must be. One at block
// 256 is required on any disc that was finalised; the copies at the end are
// what survives when the start of the disc is damaged, and are also where a
// disc written in one pass sometimes keeps the only one.
func (f *FS) findAnchor(sectors int64) ([]byte, error) {
	places := []int64{256, 512}
	if sectors > 0 {
		places = append(places, sectors-1, sectors-257)
	}
	for _, block := range places {
		if block < 0 {
			continue
		}
		buf := make([]byte, SectorSize)
		if _, err := f.r.ReadAt(buf, block*SectorSize); err != nil {
			continue
		}
		if tagID(buf) == tagAnchorPointer && validTag(buf, block) {
			return buf, nil
		}
	}
	return nil, ErrNotUDF
}

// readVolumeSequence walks the descriptors that say how the disc is laid
// out: where the partition starts, how big a block is, and where the file
// set - and through it the root directory - lives.
func (f *FS) readVolumeSequence(start, length int64) error {
	blocks := length / SectorSize
	if blocks <= 0 || blocks > 256 {
		blocks = 64
	}
	var fsd extent
	var partFound bool
	for i := int64(0); i < blocks; i++ {
		buf := make([]byte, SectorSize)
		if _, err := f.r.ReadAt(buf, (start+i)*SectorSize); err != nil {
			break
		}
		if !validTag(buf, start+i) {
			continue
		}
		switch tagID(buf) {
		case tagPrimaryVolume:
			f.vol.VolumeID = dstring(buf[24:56])
			f.vol.VolumeSetID = dstring(buf[72:200])
			f.vol.Created = timestamp(buf[376:388])
			f.vol.Modified = f.vol.Created
		case tagPartition:
			// One partition is what a disc has. A rewritable disc written
			// in packets can have a virtual one on top, which is a case
			// this reader does not pretend to handle.
			f.partStart = int64(le32(buf[188:192]))
			// How much of the disc the volume actually claims, which is
			// what "rip only the filesystem" means and is usually less
			// than the track the drive reports.
			f.vol.Sectors = f.partStart + int64(le32(buf[192:196]))
			partFound = true
		case tagLogicalVolume:
			if bs := int64(le32(buf[212:216])); bs > 0 {
				f.blockSize = bs
			}
			if id := dstring(buf[84:212]); id != "" && f.vol.VolumeID == "" {
				f.vol.VolumeID = id
			}
			f.vol.Application = regid(buf[272:304])
			fsd = longAD(buf[248:264])
		case tagTerminating:
			i = blocks
		}
	}
	if !partFound {
		return fmt.Errorf("%w: the volume names no partition", ErrNotUDF)
	}
	if fsd.length == 0 {
		return fmt.Errorf("%w: the volume names no file set", ErrNotUDF)
	}

	// The file set descriptor names the root directory, and carries the
	// name a person gave the disc.
	buf := make([]byte, SectorSize)
	if _, err := f.r.ReadAt(buf, f.byteAt(fsd.block)); err != nil {
		return fmt.Errorf("reading the file set: %w", err)
	}
	if tagID(buf) != tagFileSet {
		return fmt.Errorf("%w: the file set is not where the volume says", ErrNotUDF)
	}
	if id := dstring(buf[304:336]); id != "" {
		f.vol.VolumeID = id
	}
	f.rootICB = longAD(buf[400:416])
	return nil
}

// byteAt turns a block number inside the partition into a byte offset on
// the disc.
func (f *FS) byteAt(block int64) int64 { return (f.partStart + block) * f.blockSize }

func (f *FS) Volume() discfs.Volume { return f.vol }

// fileEntry is one file or directory as the disc records it: what it is,
// how big, when it changed, and where its contents are.
type fileEntry struct {
	isDir    bool
	symlink  bool
	size     int64
	modTime  time.Time
	mode     uint32
	extents  []extent
	inline   []byte // contents held inside the entry itself, for a tiny file
	firstLBA int64
}

// readFileEntry reads an ICB and the allocation descriptors in it.
//
// Both shapes of entry are handled. The plain File Entry is what a disc
// mastered to UDF 1.02 uses - which is every DVD-Video - and the Extended
// File Entry is what 2.00 and later use; they differ only in a run of extra
// fields in the middle, so the two offsets are all that is needed.
func (f *FS) readFileEntry(icb extent) (*fileEntry, error) {
	size := icb.length
	if size <= 0 || size > 1<<20 {
		size = f.blockSize
	}
	buf := make([]byte, roundUp(size, f.blockSize))
	if _, err := f.r.ReadAt(buf, f.byteAt(icb.block)); err != nil {
		return nil, err
	}
	var eaLen, adLen int64
	var dataAt int64
	switch tagID(buf) {
	case tagFileEntry:
		eaLen, adLen, dataAt = int64(le32(buf[168:172])), int64(le32(buf[172:176])), 176
	case tagExtendedFileEntry:
		eaLen, adLen, dataAt = int64(le32(buf[208:212])), int64(le32(buf[212:216])), 216
	default:
		return nil, fmt.Errorf("%w: sector %d is not a file entry", ErrNotFound, icb.block)
	}

	e := &fileEntry{firstLBA: f.partStart + icb.block}
	fileType := buf[16+11]
	e.isDir = fileType == fileTypeDirectory
	e.symlink = fileType == fileTypeSymlink
	e.mode = permissions(le32(buf[44:48]), e.isDir)
	if tagID(buf) == tagFileEntry {
		e.size = int64(le64(buf[56:64]))
		e.modTime = timestamp(buf[84:96])
	} else {
		e.size = int64(le64(buf[56:64]))
		e.modTime = timestamp(buf[92:104])
	}

	adStart := dataAt + eaLen
	if adStart+adLen > int64(len(buf)) {
		return nil, fmt.Errorf("the allocation descriptors of sector %d run past the entry", icb.block)
	}
	ads := buf[adStart : adStart+adLen]

	// The low three bits of the ICB flags say how the contents are
	// addressed. Type 3 is the interesting one: the file is small enough to
	// live inside its own entry, which is how UDF stores a short symlink or
	// a tiny file without spending a block on it.
	switch le16(buf[16+18:16+20]) & 0x07 {
	case 0: // short_ad
		e.extents = f.shortADs(ads)
	case 1: // long_ad
		e.extents = f.longADs(ads)
	case 3:
		e.inline = ads
		if e.size > int64(len(ads)) {
			e.size = int64(len(ads))
		}
	default:
		return nil, fmt.Errorf("sector %d uses an allocation scheme this reader does not read", icb.block)
	}
	return e, nil
}

// shortADs and longADs read a run of allocation descriptors. A descriptor
// of type 3 is not an extent at all: it points at another run of them,
// which is how a file with more pieces than fit in one entry is recorded.
func (f *FS) shortADs(b []byte) []extent {
	var out []extent
	for len(b) >= 8 {
		length := int64(le32(b[0:4]))
		kind := length >> 30
		length &= 0x3fffffff
		block := int64(le32(b[4:8]))
		b = b[8:]
		if kind == 3 {
			out = append(out, f.continuation(block, length, false)...)
			break
		}
		if length > 0 {
			out = append(out, extent{block: block, length: length, recorded: kind == 0})
		}
	}
	return out
}

func (f *FS) longADs(b []byte) []extent {
	var out []extent
	for len(b) >= 16 {
		ad := longAD(b[:16])
		kind := int64(le32(b[0:4])) >> 30
		b = b[16:]
		if kind == 3 {
			out = append(out, f.continuation(ad.block, ad.length, true)...)
			break
		}
		if ad.length > 0 {
			out = append(out, ad)
		}
	}
	return out
}

// maxContinuations bounds a chain that a damaged disc could otherwise make
// circular. A file in a thousand pieces is already past anything real.
const maxContinuations = 64

func (f *FS) continuation(block, length int64, long bool) []extent {
	var out []extent
	for range maxContinuations {
		if length <= 0 {
			return out
		}
		buf := make([]byte, roundUp(length, f.blockSize))
		if _, err := f.r.ReadAt(buf, f.byteAt(block)); err != nil {
			return out
		}
		// The run begins with an Allocation Extent Descriptor saying how
		// many bytes of descriptors follow it.
		if tagID(buf) != tagAllocationExtent {
			return out
		}
		n := int64(le32(buf[20:24]))
		if 24+n > int64(len(buf)) {
			return out
		}
		ads := buf[24 : 24+n]
		var next extent
		var found bool
		step := 8
		if long {
			step = 16
		}
		for len(ads) >= step {
			var ad extent
			var kind int64
			if long {
				ad = longAD(ads[:16])
				kind = int64(le32(ads[0:4])) >> 30
			} else {
				raw := int64(le32(ads[0:4]))
				kind = raw >> 30
				ad = extent{block: int64(le32(ads[4:8])), length: raw & 0x3fffffff, recorded: kind == 0}
			}
			ads = ads[step:]
			if kind == 3 {
				next, found = ad, true
				break
			}
			if ad.length > 0 {
				out = append(out, ad)
			}
		}
		if !found {
			return out
		}
		block, length = next.block, next.length
	}
	return out
}

// ReadDir lists one directory. Directories come before files and both are
// sorted by name, so the order is the same every time and matches what a
// file manager would show.
func (f *FS) ReadDir(dir string) ([]discfs.Entry, error) {
	entry, err := f.find(dir)
	if err != nil {
		return nil, err
	}
	if !entry.isDir {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	kids, err := f.children(entry)
	if err != nil {
		return nil, err
	}
	base := path.Clean("/" + strings.TrimPrefix(dir, "/"))
	out := make([]discfs.Entry, 0, len(kids))
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

// child is one file identifier descriptor: a name, and the ICB of what it
// names.
type child struct {
	name string
	icb  extent
	fe   *fileEntry
}

func (c child) entry(full string) discfs.Entry {
	e := discfs.Entry{Name: c.name, Path: full}
	if c.fe == nil {
		return e
	}
	e.IsDir = c.fe.isDir
	e.ModTime = c.fe.modTime
	e.Mode = c.fe.mode
	e.Extent = c.fe.firstExtent()
	if !c.fe.isDir {
		e.Size = c.fe.size
	}
	if c.fe.symlink {
		e.SymlinkTarget = c.fe.symlinkTarget()
	}
	return e
}

func (e *fileEntry) firstExtent() int64 {
	if len(e.extents) > 0 {
		return e.extents[0].block
	}
	return e.firstLBA
}

// children reads a directory's contents: a run of file identifier
// descriptors, each naming one entry, each followed by the ICB to read for
// what that entry actually is.
func (f *FS) children(dir *fileEntry) ([]child, error) {
	data, err := f.readAll(dir)
	if err != nil {
		return nil, err
	}
	var out []child
	for off := 0; off+38 <= len(data); {
		if tagID(data[off:]) != tagFileIdentifier {
			break
		}
		characteristics := data[off+18]
		nameLen := int(data[off+19])
		icb := longAD(data[off+20 : off+36])
		implUse := int(le16(data[off+36 : off+38]))
		nameAt := off + 38 + implUse
		next := roundUp4(nameAt + nameLen)
		if nameAt+nameLen > len(data) || next <= off {
			break
		}
		name := dchars(data[nameAt : nameAt+nameLen])
		off = next
		// Bit 3 marks the entry for the parent directory, which has no
		// name and is not a file anyone lists. Bit 0 marks a hidden file,
		// which is still a file: the disc is being read, not browsed by
		// someone who should be protected from it.
		if characteristics&0x08 != 0 || name == "" {
			continue
		}
		fe, err := f.readFileEntry(icb)
		if err != nil {
			// One unreadable entry should not lose the other nine hundred:
			// the name is real even when what it points at cannot be read.
			out = append(out, child{name: name, icb: icb})
			continue
		}
		out = append(out, child{name: name, icb: icb, fe: fe})
	}
	return out, nil
}

// readAll reads the whole of a file's contents into memory. It is used for
// directories, which are small, and never for file data, which is streamed.
func (f *FS) readAll(e *fileEntry) ([]byte, error) {
	if e.inline != nil {
		return e.inline, nil
	}
	var out []byte
	for _, ex := range e.extents {
		buf := make([]byte, ex.length)
		if ex.recorded {
			if _, err := f.r.ReadAt(buf, f.byteAt(ex.block)); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
		out = append(out, buf...)
	}
	return out, nil
}

// symlinkTarget assembles the path a symbolic link points at. UDF records
// it as a sequence of components rather than as a string, so that a link
// made on one system means the same thing on another.
func (e *fileEntry) symlinkTarget() string {
	b := e.inline
	if b == nil {
		return ""
	}
	var parts []string
	for len(b) >= 4 {
		kind, length := b[0], int(b[1])
		if 4+length > len(b) {
			break
		}
		switch kind {
		case 1:
			parts = append(parts, "")
		case 2:
			parts = append(parts, "/")
		case 3:
			parts = append(parts, "..")
		case 4:
			parts = append(parts, ".")
		case 5:
			parts = append(parts, dchars(b[4:4+length]))
		}
		b = b[roundUp4(4+length):]
	}
	return strings.TrimPrefix(strings.Join(parts, "/"), "//")
}

// find walks the tree to p, which must be an absolute, slash-separated
// path. Names are matched case-insensitively, as on the ISO 9660 side: a
// path typed from a listing should work whichever way it was capitalised.
func (f *FS) find(p string) (*fileEntry, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	cur := f.rootEntry
	if p == "/" {
		return cur, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		if !cur.isDir {
			return nil, ErrNotFound
		}
		kids, err := f.children(cur)
		if err != nil {
			return nil, err
		}
		found := false
		for _, k := range kids {
			if k.fe != nil && strings.EqualFold(k.name, part) {
				cur, found = k.fe, true
				break
			}
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	return cur, nil
}

// Stat describes one path.
func (f *FS) Stat(p string) (discfs.Entry, error) {
	fe, err := f.find(p)
	if err != nil {
		return discfs.Entry{}, err
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	c := child{name: path.Base(clean), fe: fe}
	e := c.entry(clean)
	if clean == "/" {
		e.Name, e.IsDir = "/", true
	}
	return e, nil
}

// Open returns the contents of one file. Nothing is buffered up front, so
// opening a 4 GB file costs nothing and a range request for the middle of
// it seeks there directly - which is what playing a video off a disc is.
func (f *FS) Open(p string) (discfs.File, discfs.Entry, error) {
	fe, err := f.find(p)
	if err != nil {
		return nil, discfs.Entry{}, err
	}
	if fe.isDir {
		return nil, discfs.Entry{}, ErrIsDir
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	e := child{name: path.Base(clean), fe: fe}.entry(clean)
	return f.reader(fe), e, nil
}

// Walk visits every entry in the tree, depth first, stopping at the first
// error fn returns.
func (f *FS) Walk(root string, fn func(discfs.Entry) error) error {
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

/* ---------- the small readers ---------- */

func tagID(b []byte) int {
	if len(b) < 16 {
		return 0
	}
	return int(le16(b[0:2]))
}

// validTag checks a descriptor is the one that belongs where it was found.
// The checksum catches a misread sector; the location catches a structure
// read from the wrong place, which is what a disc with two sessions or a
// wrong anchor looks like.
func validTag(b []byte, at int64) bool {
	if len(b) < 16 {
		return false
	}
	var sum byte
	for i, c := range b[:16] {
		if i == 4 {
			continue
		}
		sum += c
	}
	if sum != b[4] {
		return false
	}
	return int64(le32(b[12:16])) == at
}

// longAD reads a long allocation descriptor: a length, and a block within a
// partition.
func longAD(b []byte) extent {
	if len(b) < 16 {
		return extent{}
	}
	raw := int64(le32(b[0:4]))
	return extent{
		block:    int64(le32(b[4:8])),
		length:   raw & 0x3fffffff,
		recorded: raw>>30 == 0,
	}
}

// dstring reads a UDF identifier: OSTA compressed Unicode, with the length
// in the last byte of the field rather than at the front.
func dstring(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	n := int(b[len(b)-1])
	if n <= 0 || n > len(b)-1 {
		return strings.TrimRight(dchars(b[:len(b)-1]), "\x00 ")
	}
	return strings.TrimRight(dchars(b[:n]), "\x00 ")
}

// dchars reads OSTA compressed Unicode: one leading byte saying whether the
// characters that follow are eight or sixteen bits wide. This is why a UDF
// disc can be labelled in Cyrillic and an ISO 9660 one cannot.
func dchars(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 8:
		// Each byte is a code point, not a byte of UTF-8.
		return string([]rune(latin(b[1:])))
	case 16:
		units := make([]uint16, 0, (len(b)-1)/2)
		for i := 1; i+1 < len(b); i += 2 {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return strings.TrimRight(string(utf16.Decode(units)), "\x00")
	}
	return ""
}

func latin(b []byte) string {
	r := make([]rune, 0, len(b))
	for _, c := range b {
		if c == 0 {
			continue
		}
		r = append(r, rune(c))
	}
	return string(r)
}

// regid reads a registered identifier: an eight-bit flag byte followed by a
// plain identifier, which is the name of whatever wrote the disc.
func regid(b []byte) string {
	if len(b) < 24 {
		return ""
	}
	return strings.TrimRight(latin(b[1:24]), "\x00 ")
}

// timestamp reads a UDF timestamp. The time zone is recorded as minutes
// from UTC in the low twelve bits, as a signed value; a disc that does not
// know its own zone says so, and is then read as UTC.
func timestamp(b []byte) time.Time {
	if len(b) < 12 {
		return time.Time{}
	}
	year := int(int16(le16(b[2:4])))
	if year == 0 {
		return time.Time{}
	}
	loc := time.UTC
	tz := int16(le16(b[0:2])) & 0x0fff
	if tz&0x0800 != 0 {
		tz |= ^0x0fff // sign-extend the twelve-bit field
	}
	if raw := le16(b[0:2]) >> 12; raw == 1 && tz != -2047 {
		loc = time.FixedZone("", int(tz)*60)
	}
	return time.Date(year, time.Month(b[4]), int(b[5]),
		int(b[6]), int(b[7]), int(b[8]), int(b[9])*10_000_000, loc)
}

// permissions turns UDF's permission bits into the mode the rest of ripperX
// shows. UDF keeps them per owner, group and other, in the opposite order
// to the one Unix writes them in.
func permissions(p uint32, isDir bool) uint32 {
	bit := func(shift uint, mask uint32) uint32 {
		var m uint32
		if p&(mask<<shift) != 0 {
			m |= 4 // read
		}
		if p&((mask<<1)<<shift) != 0 {
			m |= 2 // write
		}
		if p&((mask<<4)<<shift) != 0 {
			m |= 1 // execute
		}
		return m
	}
	mode := bit(10, 1)<<6 | bit(5, 1)<<3 | bit(0, 1)
	if isDir {
		mode |= 0o40000
	}
	return mode
}

func roundUp(n, to int64) int64 {
	if to <= 0 {
		return n
	}
	return (n + to - 1) / to * to
}

func roundUp4(n int) int { return (n + 3) &^ 3 }

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(le32(b[0:4])) | uint64(le32(b[4:8]))<<32
}
