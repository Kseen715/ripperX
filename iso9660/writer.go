package iso9660

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// Writing an ISO 9660 image is the other half of reading one, and it is here
// for one reason: a folder taken off a disc should be able to come back as a
// disc. A zip of a folder is a file about a disc; an .iso of it is the disc
// again, and it is the only one of the formats ripperX produces that can be
// burned without being unpacked first.
//
// The image carries all three naming schemes this package reads, for the
// same reason it reads all three: the 8.3 names every reader understands,
// Joliet for the real names, and Rock Ridge for the names, the permissions
// and the symlinks a Unix machine expects. Nothing else writes them here, so
// what this produces is exactly what Open reads back.
//
// Everything is planned before a byte is written. The size of every file is
// known in advance - it comes off the disc's own directory records - so
// every extent can be placed first and the image then written straight out
// in one pass, with nothing seekable and nothing staged. That is what lets
// an image be produced into a socket or onto a share.

// Item is one file, directory or symlink to put in the image. Path is
// slash-separated and relative to the image's root, with no leading slash.
type Item struct {
	Path    string
	IsDir   bool
	Size    int64
	ModTime time.Time
	// Mode is a Unix st_mode, as Rock Ridge records it. Zero means "not
	// known", and a sensible one is invented.
	Mode uint32
	// Symlink, when set, makes this a symbolic link to that target and Size
	// is ignored.
	Symlink string
}

// Options are what the volume says about itself. Everything is optional
// except the identifier, which is invented when it is left empty because a
// volume with no name is one nothing will mount by label.
type Options struct {
	VolumeID    string
	SystemID    string
	Publisher   string
	Preparer    string
	Application string
	Created     time.Time
}

// The limits each tree imposes on a name. ISO 9660 level 2 allows 31
// characters; a file spends one of them on the ";1" it must end with, so 30
// are left for the name itself. Joliet allows 64 UTF-16 code units, which is
// the limit Windows has always imposed on a disc.
const (
	isoNameMax    = 30
	isoDirNameMax = 31
	jolietNameMax = 64
	// maxRecord is the largest a directory record may be: the length is one
	// byte and the record must be even.
	maxRecord = 254
)

// The two trees. Every directory is laid out twice - once with the 8.3
// names and once with the Joliet ones - and both point at the same file
// extents, so the second tree costs directory records and not a second copy
// of the data.
const (
	treeISO = iota
	treeJoliet
	numTrees
)

// Unix file type bits, as Rock Ridge's PX entry records them.
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000
	modeTypeAll = 0o170000
)

var errTooDeep = errors.New("this tree is too deep to write as an ISO 9660 image")

// maxDepth is how far down the tree may go. ISO 9660 itself allows eight
// levels and relocates anything deeper; nothing that reads these images
// today minds a deeper tree, so they are written as they are - but a limit
// is still needed, because the path table's parent numbers are 16 bits and
// a tree without one is a tree that can exhaust memory.
const maxDepth = 64

type node struct {
	name   string
	isDir  bool
	item   Item
	kids   []*node
	parent *node

	// id is this node's identifier in each tree, ext and length the extent
	// and byte length of its own directory in each tree, and num its number
	// in that tree's path table.
	id     [numTrees]string
	ext    [numTrees]int64
	length [numTrees]int64
	num    [numTrees]int

	// rr is the Rock Ridge system use area for this node's record in its
	// parent's directory. It is built during planning, before any extent is
	// known, because nothing Rock Ridge records is an extent.
	rr []byte

	dataExtent int64
	dataLen    int64
}

// Layout is a planned image: every extent decided, nothing written. It
// knows how large the image will be before it exists, which is what lets a
// rip refuse for want of room and a download declare a length.
type Layout struct {
	opt   Options
	root  *node
	dirs  [numTrees][]*node
	files []*node

	pathTableSize [numTrees]int64
	pathTableLBA  [numTrees][2]int64 // little-endian copy, then big-endian
	sectors       int64
}

// Plan works out where everything goes. It fails only on input that cannot
// be a filesystem at all - a path that escapes the root, a tree deeper than
// anything will read - rather than on anything about the names, which are
// made to fit.
func Plan(items []Item, opt Options) (*Layout, error) {
	l := &Layout{opt: opt, root: &node{isDir: true, name: ""}}
	if opt.Created.IsZero() {
		l.opt.Created = time.Now()
	}
	l.root.item.ModTime = l.opt.Created
	for _, it := range items {
		if err := l.add(it); err != nil {
			return nil, err
		}
	}
	l.nameTree(l.root)
	l.rockRidge(l.root)
	l.place()
	return l, nil
}

// Size is how many bytes the finished image will be.
func (l *Layout) Size() int64 { return l.sectors * BlockSize }

// Sectors is the same figure in the unit a disc is measured in.
func (l *Layout) Sectors() int64 { return l.sectors }

// Files lists the files whose contents Write will ask for, in the order it
// will ask. Directories and symlinks are not among them: neither has any
// contents to fetch.
func (l *Layout) Files() []Item {
	out := make([]Item, 0, len(l.files))
	for _, n := range l.files {
		if n.item.Symlink == "" && !n.isDir {
			out = append(out, n.item)
		}
	}
	return out
}

// add puts one item into the tree, creating the directories above it. A
// directory that arrives both implicitly and explicitly is one directory:
// the explicit one's metadata wins, because it is the one that came off the
// disc.
func (l *Layout) add(it Item) error {
	clean := strings.Trim(strings.ReplaceAll(it.Path, "\\", "/"), "/")
	if clean == "" {
		return nil // the root itself; it already exists
	}
	parts := strings.Split(clean, "/")
	if len(parts) > maxDepth {
		return fmt.Errorf("%s: %w", it.Path, errTooDeep)
	}
	cur := l.root
	for i, part := range parts {
		if part == "." || part == ".." {
			return fmt.Errorf("%q is not a name that can go in an image", it.Path)
		}
		last := i == len(parts)-1
		child := cur.find(part)
		if child == nil {
			child = &node{name: part, parent: cur, isDir: !last || it.IsDir}
			child.item = Item{Path: strings.Join(parts[:i+1], "/"), IsDir: child.isDir}
			cur.kids = append(cur.kids, child)
		}
		if last {
			if it.IsDir || it.Symlink != "" || !child.isDir {
				child.isDir = it.IsDir
				child.item = it
			}
			// A path that is a file here and a directory there cannot be
			// both; the directory wins, since something is inside it.
			if len(child.kids) > 0 {
				child.isDir = true
			}
		}
		cur = child
	}
	return nil
}

func (n *node) find(name string) *node {
	for _, k := range n.kids {
		if strings.EqualFold(k.name, name) {
			return k
		}
	}
	return nil
}

/* ---------- names ---------- */

// nameTree gives every child an identifier in each tree and sorts the
// children the way each tree wants them. The two orders differ, which is
// why the sort is per tree and the records are laid out twice.
func (l *Layout) nameTree(dir *node) {
	// A stable starting order, so an image built twice from the same disc
	// is byte for byte the same image.
	sort.SliceStable(dir.kids, func(i, j int) bool { return dir.kids[i].name < dir.kids[j].name })

	takenISO := map[string]bool{}
	takenJol := map[string]bool{}
	for _, k := range dir.kids {
		k.id[treeISO] = unique(isoIdentifier(k.name, k.isDir), takenISO, k.isDir, false)
		k.id[treeJoliet] = unique(jolietIdentifier(k.name), takenJol, k.isDir, true)
	}
	for _, k := range dir.kids {
		if k.isDir {
			l.nameTree(k)
		}
	}
}

// isoIdentifier turns a name into the uppercase form every reader
// understands: d-characters only, one dot, and the ";1" a file must carry.
func isoIdentifier(name string, isDir bool) string {
	stem, ext := name, ""
	if !isDir {
		if i := strings.LastIndexByte(name, '.'); i > 0 {
			stem, ext = name[:i], name[i+1:]
		}
	}
	stem = dChars(stem)
	ext = dChars(ext)
	if stem == "" {
		stem = "_"
	}
	if isDir {
		if len(stem) > isoDirNameMax {
			stem = stem[:isoDirNameMax]
		}
		return stem
	}
	// Three characters of extension is what every reader expects, and the
	// budget is 30 characters between the two parts.
	if len(ext) > 3 {
		ext = ext[:3]
	}
	room := isoNameMax
	if ext != "" {
		room -= len(ext) + 1
	}
	if len(stem) > room {
		stem = stem[:room]
	}
	if ext == "" {
		return stem + ";1"
	}
	return stem + "." + ext + ";1"
}

// dChars is the character set ISO 9660 allows in a name: uppercase letters,
// digits and the underscore. Everything else becomes an underscore rather
// than disappearing, so two names that differ only in punctuation still
// differ here.
func dChars(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// jolietIdentifier keeps the real name, minus the handful of characters
// Joliet reserves and whatever does not fit in 64 UTF-16 code units.
func jolietIdentifier(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '*', '/', ':', ';', '?', '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if u := utf16.Encode([]rune(out)); len(u) > jolietNameMax {
		out = string(utf16.Decode(u[:jolietNameMax]))
	}
	if out == "" {
		out = "_"
	}
	return out
}

// unique makes an identifier one nothing else in this directory has, by
// putting a number before the extension. Two files called "Photo (1).jpg"
// and "Photo [1].jpg" flatten to the same 8.3 name, and silently writing
// one over the other is the one outcome that must not happen.
func unique(id string, taken map[string]bool, isDir, joliet bool) string {
	key := strings.ToUpper(id)
	if !taken[key] {
		taken[key] = true
		return id
	}
	stem, tail := id, ""
	if !isDir && !joliet {
		if i := strings.LastIndex(id, ";"); i > 0 {
			stem, tail = id[:i], id[i:]
		}
	}
	base, ext := stem, ""
	if i := strings.LastIndexByte(stem, '.'); i > 0 {
		base, ext = stem[:i], stem[i:]
	}
	limit := isoNameMax
	if isDir {
		limit = isoDirNameMax
	}
	if joliet {
		limit = jolietNameMax
	}
	for i := 2; i < 1<<16; i++ {
		suffix := fmt.Sprintf("~%d", i)
		room := limit - len(ext) - len(suffix)
		trimmed := base
		if room < 1 {
			room = 1
		}
		if len(trimmed) > room {
			trimmed = trimmed[:room]
		}
		try := trimmed + suffix + ext + tail
		if k := strings.ToUpper(try); !taken[k] {
			taken[k] = true
			return try
		}
	}
	taken[key] = true
	return id
}

/* ---------- Rock Ridge ---------- */

// rockRidge builds the system use area for every record in the primary
// tree. Joliet carries no Rock Ridge - the two are alternatives, and every
// reader that looks for one ignores the other.
//
// What goes in is decided by what fits. A directory record is 254 bytes at
// the most, and a long name plus a symlink target can want more than that,
// so the entries are added in the order they matter: a symlink's target
// first, because a link without one is a lie; then the mode, then the real
// name, then the timestamp. Whatever is left out is still in the Joliet
// tree, which has room for it.
func (l *Layout) rockRidge(dir *node) {
	for _, k := range dir.kids {
		k.rr = k.rockRidgeArea()
		if k.isDir {
			l.rockRidge(k)
		}
	}
}

func (n *node) rockRidgeArea() []byte {
	// The record's own fields, so what is left for Rock Ridge is known.
	idLen := len(n.id[treeISO])
	room := maxRecord - (33 + idLen + recordPad(idLen))

	var out []byte
	fits := func(entry []byte) bool { return len(out)+len(entry) <= room }

	if n.item.Symlink != "" {
		for _, e := range symlinkEntries(n.item.Symlink) {
			if !fits(e) {
				break
			}
			out = append(out, e...)
		}
	}
	if px := pxEntry(n.mode()); fits(px) {
		out = append(out, px...)
	}
	for _, e := range nameEntries(n.name) {
		if !fits(e) {
			break
		}
		out = append(out, e...)
	}
	if tf := tfEntry(n.item.ModTime); fits(tf) {
		out = append(out, tf...)
	}
	if len(out)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// mode is the st_mode to record. A disc with no Rock Ridge on it gives
// none, and inventing a plausible one is better than writing a file that
// unpacks with no permissions at all.
func (n *node) mode() uint32 {
	m := n.item.Mode
	if m&modeTypeAll != 0 {
		return m
	}
	perm := m & 0o7777
	switch {
	case n.item.Symlink != "":
		return modeSymlink | 0o777
	case n.isDir:
		if perm == 0 {
			perm = 0o755
		}
		return modeDir | perm
	default:
		if perm == 0 {
			perm = 0o444
		}
		return modeRegular | perm
	}
}

func suspHeader(sig string, length int) []byte {
	return []byte{sig[0], sig[1], byte(length), 1}
}

// pxEntry records the POSIX attributes: the mode, the link count and the
// owner. The owner is root because nothing on a disc has one - a disc has
// no /etc/passwd to resolve a number against, and zero is the number every
// mastering program writes.
func pxEntry(mode uint32) []byte {
	e := make([]byte, 0, 36)
	e = append(e, suspHeader("PX", 36)...)
	e = appendBoth32(e, mode)
	e = appendBoth32(e, 1) // links
	e = appendBoth32(e, 0) // uid
	e = appendBoth32(e, 0) // gid
	return e
}

// nameEntries spell out the real name, split across as many NM entries as
// it takes. Each carries at most 250 bytes of it and all but the last set
// the CONTINUE flag.
func nameEntries(name string) [][]byte {
	const chunk = 250
	var out [][]byte
	b := []byte(name)
	for len(b) > 0 {
		n := len(b)
		if n > chunk {
			n = chunk
		}
		flags := byte(0)
		if n < len(b) {
			flags = 0x01 // CONTINUE
		}
		e := append(suspHeader("NM", 5+n), flags)
		out = append(out, append(e, b[:n]...))
		b = b[n:]
	}
	return out
}

// tfEntry records the modification time in the same seven-byte form the
// directory record itself uses, which is the one every reader of Rock Ridge
// expects to find here.
func tfEntry(t time.Time) []byte {
	e := append(suspHeader("TF", 12), 0x02) // bit 1: modify
	ts := make([]byte, 7)
	putDirTime(ts, t)
	return append(e, ts...)
}

// symlinkEntries spell out a target as the components it is made of, which
// is how Rock Ridge records one: "." and ".." and the root have flags of
// their own rather than being written out.
func symlinkEntries(target string) [][]byte {
	var comps [][]byte
	if strings.HasPrefix(target, "/") {
		comps = append(comps, []byte{0x08, 0})
	}
	for _, part := range strings.Split(strings.Trim(target, "/"), "/") {
		switch part {
		case "":
		case ".":
			comps = append(comps, []byte{0x02, 0})
		case "..":
			comps = append(comps, []byte{0x04, 0})
		default:
			b := []byte(part)
			if len(b) > 250 {
				b = b[:250]
			}
			comps = append(comps, append([]byte{0x00, byte(len(b))}, b...))
		}
	}
	// One SL entry holds as many components as fit in its 255 bytes; the
	// rest go into further entries marked CONTINUE.
	var out [][]byte
	var cur []byte
	flush := func(more bool) {
		if len(cur) == 0 {
			return
		}
		flags := byte(0)
		if more {
			flags = 0x01
		}
		e := append(suspHeader("SL", 5+len(cur)), flags)
		out = append(out, append(e, cur...))
		cur = nil
	}
	for i, c := range comps {
		if len(cur)+len(c) > 250 {
			flush(true)
		}
		cur = append(cur, c...)
		if i == len(comps)-1 {
			flush(false)
		}
	}
	return out
}

// rootSUSP is what goes in the root directory's own "." record: the
// declaration that this volume carries SUSP at all, and the one that names
// Rock Ridge as the extension using it. Without these a reader is entitled
// to ignore every annotation in the image.
//
// The extension is registered as IEEE_P1282 - Rock Ridge 1.12 - rather than
// RRIP_1991A, because 1.09 also wants an "RR" entry in every record saying
// which annotations that record carries, and that is a byte of bookkeeping
// per file to tell a reader something it can see for itself.
func rootSUSP(mode uint32, t time.Time) []byte {
	const erID = "IEEE_P1282"
	out := append(suspHeader("SP", 7), 0xBE, 0xEF, 0x00)
	er := append(suspHeader("ER", 8+len(erID)),
		byte(len(erID)), 0, 0, 1)
	out = append(out, append(er, erID...)...)
	out = append(out, pxEntry(mode)...)
	out = append(out, tfEntry(t)...)
	if len(out)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

/* ---------- placing everything ---------- */

// recordPad is the byte a directory record needs after an even-length name to
// keep what follows it even-aligned.
func recordPad(idLen int) int {
	if idLen%2 == 0 {
		return 1
	}
	return 0
}

func recordLen(idLen, suspLen int) int { return 33 + idLen + recordPad(idLen) + suspLen }

// dirLength is how many bytes one directory's records take in one tree. A
// record may not straddle a sector, so a record that would is pushed to the
// start of the next one and the gap is left as zeroes.
func (l *Layout) dirLength(n *node, t int) int64 {
	var susp, suspParent []byte
	if t == treeISO {
		susp = n.selfSUSP()
		suspParent = n.parentSUSP()
	}
	off := int64(recordLen(1, len(susp)) + recordLen(1, len(suspParent)))
	for _, k := range n.kids {
		r := int64(recordLen(len(k.id[t]), len(k.rrFor(t))))
		if off%BlockSize+r > BlockSize {
			off += BlockSize - off%BlockSize
		}
		off += r
	}
	return roundUp(off, BlockSize)
}

func (n *node) rrFor(t int) []byte {
	if t == treeISO {
		return n.rr
	}
	return nil
}

// selfSUSP is the system use area of a directory's own "." record, and
// parentSUSP that of its "..". Only the root's "." carries the declarations;
// every other one carries the attributes and nothing else.
func (n *node) selfSUSP() []byte {
	if n.parent == nil {
		return rootSUSP(n.mode(), n.item.ModTime)
	}
	return evenPad(append(pxEntry(n.mode()), tfEntry(n.item.ModTime)...))
}

func (n *node) parentSUSP() []byte {
	p := n
	if n.parent != nil {
		p = n.parent
	}
	return evenPad(append(pxEntry(p.mode()), tfEntry(p.item.ModTime)...))
}

func evenPad(b []byte) []byte {
	if len(b)%2 == 1 {
		return append(b, 0)
	}
	return b
}

func roundUp(n, to int64) int64 { return (n + to - 1) / to * to }

// place decides where every structure goes. The order is the order it is
// written in, because the image is produced in one pass with nothing
// seekable underneath it.
func (l *Layout) place() {
	l.root.isDir = true
	for t := range numTrees {
		l.dirs[t] = l.order(t)
		for i, d := range l.dirs[t] {
			d.num[t] = i + 1
		}
		l.pathTableSize[t] = l.pathTable(t, 0, true, nil)
	}

	// Sixteen sectors of system area, the two volume descriptors and their
	// terminator.
	lba := int64(systemArea + 3)
	for t := range numTrees {
		size := roundUp(l.pathTableSize[t], BlockSize) / BlockSize
		for copyIdx := range 2 {
			l.pathTableLBA[t][copyIdx] = lba
			lba += size
		}
	}
	for t := range numTrees {
		for _, d := range l.dirs[t] {
			d.length[t] = l.dirLength(d, t)
			d.ext[t] = lba
			lba += d.length[t] / BlockSize
		}
	}
	// The file contents come last and are shared by both trees.
	l.files = nil
	l.collectFiles(l.root)
	for _, f := range l.files {
		f.dataExtent = lba
		f.dataLen = f.item.Size
		if f.item.Symlink != "" {
			f.dataLen = 0
		}
		lba += roundUp(f.dataLen, BlockSize) / BlockSize
	}
	// A volume of fewer than one sector is not a volume, and a handful of
	// readers dislike an image with no slack at the end of it.
	if lba < systemArea+4 {
		lba = systemArea + 4
	}
	l.sectors = lba
}

// order lists the directories the way a path table wants them: the root,
// then every directory at each level in turn, and within a level by parent
// and then by identifier.
func (l *Layout) order(t int) []*node {
	out := []*node{l.root}
	for i := 0; i < len(out); i++ {
		dir := out[i]
		kids := make([]*node, 0, len(dir.kids))
		for _, k := range dir.kids {
			if k.isDir {
				kids = append(kids, k)
			}
		}
		sort.SliceStable(kids, func(a, b int) bool {
			return identifierLess(kids[a].id[t], kids[b].id[t])
		})
		out = append(out, kids...)
	}
	return out
}

func (l *Layout) collectFiles(dir *node) {
	kids := append([]*node(nil), dir.kids...)
	sort.SliceStable(kids, func(a, b int) bool {
		return identifierLess(kids[a].id[treeISO], kids[b].id[treeISO])
	})
	for _, k := range kids {
		if k.isDir {
			l.collectFiles(k)
			continue
		}
		l.files = append(l.files, k)
	}
}

// identifierLess is ISO 9660's ordering: the identifiers compared as if the
// shorter were padded with spaces, which puts "AB" before "ABC" and both
// before "AC".
func identifierLess(a, b string) bool {
	for i := 0; i < len(a) || i < len(b); i++ {
		ca, cb := byte(' '), byte(' ')
		if i < len(a) {
			ca = a[i]
		}
		if i < len(b) {
			cb = b[i]
		}
		if ca != cb {
			return ca < cb
		}
	}
	return false
}

/* ---------- writing ---------- */

// Write produces the image. open is called once per file, in the order
// Files lists them, and must return its contents; a reader that ends early
// is padded out, because a header has already promised a length and a
// damaged sector must not misalign everything after it.
func (l *Layout) Write(ctx context.Context, w io.Writer, open func(Item) (io.Reader, error)) error {
	zero := make([]byte, BlockSize)
	for range systemArea {
		if _, err := w.Write(zero); err != nil {
			return err
		}
	}
	if _, err := w.Write(l.descriptor(treeISO)); err != nil {
		return err
	}
	if _, err := w.Write(l.descriptor(treeJoliet)); err != nil {
		return err
	}
	term := make([]byte, BlockSize)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1
	if _, err := w.Write(term); err != nil {
		return err
	}

	// The little-endian copy of each path table, then the big-endian one,
	// in the order the descriptors point at them.
	for t := range numTrees {
		for endian := range 2 {
			buf := make([]byte, roundUp(l.pathTableSize[t], BlockSize))
			l.pathTable(t, endian, false, func(off int, b []byte) { copy(buf[off:], b) })
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
	}

	for t := range numTrees {
		for _, d := range l.dirs[t] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := w.Write(l.directory(d, t)); err != nil {
				return err
			}
		}
	}

	for _, f := range l.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.dataLen == 0 {
			continue
		}
		src, err := open(f.item)
		if err != nil {
			return fmt.Errorf("%s: %w", f.item.Path, err)
		}
		if err := copyExact(ctx, w, src, f.dataLen); err != nil {
			return fmt.Errorf("%s: %w", f.item.Path, err)
		}
		if rest := roundUp(f.dataLen, BlockSize) - f.dataLen; rest > 0 {
			if _, err := w.Write(zero[:rest]); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyExact writes exactly want bytes, padding a reader that ends early. A
// file in a damaged sector is a short read, and an image whose extents do
// not match its directory records is an image nothing can mount.
func copyExact(ctx context.Context, dst io.Writer, src io.Reader, want int64) error {
	buf := make([]byte, 256<<10)
	var done int64
	for done < want {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if done+int64(n) > want {
				n = int(want - done)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
		}
		if err != nil {
			break
		}
	}
	zero := make([]byte, 32<<10)
	for done < want {
		n := int64(len(zero))
		if rest := want - done; rest < n {
			n = rest
		}
		if _, err := dst.Write(zero[:n]); err != nil {
			return err
		}
		done += n
	}
	return nil
}

// pathTable writes, or only measures, one path table. endian is 0 for the
// little-endian copy and 1 for the big-endian one; they differ in nothing
// else.
func (l *Layout) pathTable(t, endian int, measure bool, emit func(off int, b []byte)) int64 {
	off := 0
	for _, d := range l.dirs[t] {
		id := d.id[t]
		if d.parent == nil {
			id = "\x00"
		}
		if t == treeJoliet && d.parent != nil {
			id = string(ucs2(id))
		}
		rec := make([]byte, 8+len(id)+recordPad(len(id)))
		rec[0] = byte(len(id))
		if endian == 0 {
			putLE32(rec[2:6], uint32(d.ext[t]))
			putLE16(rec[6:8], uint16(d.parentNumber(t)))
		} else {
			putBE32(rec[2:6], uint32(d.ext[t]))
			putBE16(rec[6:8], uint16(d.parentNumber(t)))
		}
		copy(rec[8:], id)
		if !measure {
			emit(off, rec)
		}
		off += len(rec)
	}
	return int64(off)
}

func (n *node) parentNumber(t int) int {
	if n.parent == nil {
		return 1
	}
	return n.parent.num[t]
}

// directory renders one directory's extent: its own record, its parent's,
// and one for each child, none of them straddling a sector.
func (l *Layout) directory(n *node, t int) []byte {
	buf := make([]byte, n.length[t])
	off := 0

	var selfSUSP, parentSUSP []byte
	if t == treeISO {
		selfSUSP, parentSUSP = n.selfSUSP(), n.parentSUSP()
	}
	self := newDirRecord("\x00", n.ext[t], n.length[t], true, n.item.ModTime, selfSUSP)
	copy(buf[off:], self)
	off += len(self)

	up := n
	if n.parent != nil {
		up = n.parent
	}
	parent := newDirRecord("\x01", up.ext[t], up.length[t], true, up.item.ModTime, parentSUSP)
	copy(buf[off:], parent)
	off += len(parent)

	kids := append([]*node(nil), n.kids...)
	sort.SliceStable(kids, func(a, b int) bool {
		return identifierLess(kids[a].id[t], kids[b].id[t])
	})
	for _, k := range kids {
		id := k.id[t]
		if t == treeJoliet {
			id = string(ucs2(id))
		}
		extent, length := k.dataExtent, k.dataLen
		if k.isDir {
			extent, length = k.ext[t], k.length[t]
		}
		rec := newDirRecord(id, extent, length, k.isDir, k.item.ModTime, k.rrFor(t))
		if off%BlockSize+len(rec) > BlockSize {
			off += BlockSize - off%BlockSize
		}
		copy(buf[off:], rec)
		off += len(rec)
	}
	return buf
}

func newDirRecord(id string, extent, length int64, isDir bool, mod time.Time, susp []byte) []byte {
	n := recordLen(len(id), len(susp))
	b := make([]byte, n)
	b[0] = byte(n)
	putBoth32(b[2:10], uint32(extent))
	putBoth32(b[10:18], uint32(length))
	putDirTime(b[18:25], mod)
	if isDir {
		b[25] = 0x02
	}
	putBoth16(b[28:32], 1)
	b[32] = byte(len(id))
	copy(b[33:], id)
	copy(b[33+len(id)+recordPad(len(id)):], susp)
	return b
}

/* ---------- volume descriptors ---------- */

func (l *Layout) descriptor(t int) []byte {
	b := make([]byte, BlockSize)
	joliet := t == treeJoliet
	b[0] = 1
	if joliet {
		b[0] = 2
	}
	copy(b[1:6], "CD001")
	b[6] = 1

	volumeID := l.opt.VolumeID
	if volumeID == "" {
		volumeID = "RIPPERX"
	}
	putText(b[8:40], l.opt.SystemID, joliet)
	putText(b[40:72], volumeID, joliet)
	putBoth32(b[80:88], uint32(l.sectors))
	if joliet {
		// The escape sequence that says these names are UCS-2 level 3,
		// which is the one every Joliet reader looks for.
		copy(b[88:120], "%/E")
	}
	putBoth16(b[120:124], 1) // volume set size
	putBoth16(b[124:128], 1) // volume sequence number
	putBoth16(b[128:132], BlockSize)
	putBoth32(b[132:140], uint32(l.pathTableSize[t]))
	putLE32(b[140:144], uint32(l.pathTableLBA[t][0]))
	putBE32(b[148:152], uint32(l.pathTableLBA[t][1]))

	root := newDirRecord("\x00", l.root.ext[t], l.root.length[t], true, l.opt.Created, nil)
	copy(b[156:190], root)

	putText(b[190:318], "", joliet)
	putText(b[318:446], l.opt.Publisher, joliet)
	putText(b[446:574], l.opt.Preparer, joliet)
	putText(b[574:702], l.opt.Application, joliet)
	for _, r := range [][2]int{{702, 739}, {739, 776}, {776, 813}} {
		putText(b[r[0]:r[1]], "", joliet)
	}
	created := l.opt.Created
	if created.IsZero() {
		created = time.Now()
	}
	putVolumeTime(b[813:830], created)
	putVolumeTime(b[830:847], created)
	putVolumeTime(b[847:864], time.Time{})
	putVolumeTime(b[864:881], time.Time{})
	b[881] = 1
	return b
}

// putText writes one of the descriptor's identifier fields, padded the way
// the field wants: with spaces in the primary descriptor, and with UCS-2
// spaces in the Joliet one.
func putText(dst []byte, s string, joliet bool) {
	if !joliet {
		for i := range dst {
			dst[i] = ' '
		}
		copy(dst, s)
		return
	}
	for i := 0; i+1 < len(dst); i += 2 {
		dst[i], dst[i+1] = 0x00, 0x20
	}
	copy(dst, ucs2(s))
}

// ucs2 is a name as Joliet stores one: UTF-16, big-endian.
func ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = append(b, byte(c>>8), byte(c))
	}
	return b
}

func putLE16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func putBE16(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putBE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// Both-endian is how ISO 9660 stores every number: little-endian, then the
// same value big-endian, so a reader of either persuasion finds one it can
// use without knowing which machine wrote the disc.
func putBoth16(b []byte, v uint16) { putLE16(b[0:2], v); putBE16(b[2:4], v) }
func putBoth32(b []byte, v uint32) { putLE32(b[0:4], v); putBE32(b[4:8], v) }

func appendBoth32(b []byte, v uint32) []byte {
	var tmp [8]byte
	putBoth32(tmp[:], v)
	return append(b, tmp[:]...)
}

// putDirTime writes the seven-byte form a directory record uses. A time
// outside what the format can hold - and the zero time is one - is written
// as all zeroes, which is how the format says "not recorded".
func putDirTime(b []byte, t time.Time) {
	for i := range b[:7] {
		b[i] = 0
	}
	if t.IsZero() || t.Year() < 1900 || t.Year() > 2155 {
		return
	}
	_, off := t.Zone()
	b[0] = byte(t.Year() - 1900)
	b[1] = byte(t.Month())
	b[2] = byte(t.Day())
	b[3] = byte(t.Hour())
	b[4] = byte(t.Minute())
	b[5] = byte(t.Second())
	b[6] = byte(int8(clampOffset(off / 900)))
}

func clampOffset(q int) int {
	if q < -48 {
		return -48
	}
	if q > 52 {
		return 52
	}
	return q
}

// putVolumeTime writes the seventeen-byte decimal form a volume descriptor
// uses. The zero time is written as sixteen ASCII zeroes, which is the
// format's way of saying the field is not specified.
func putVolumeTime(b []byte, t time.Time) {
	if t.IsZero() {
		for i := range b[:16] {
			b[i] = '0'
		}
		b[16] = 0
		return
	}
	_, off := t.Zone()
	copy(b, fmt.Sprintf("%04d%02d%02d%02d%02d%02d%02d",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/int(10*time.Millisecond)))
	b[16] = byte(int8(clampOffset(off / 900)))
}
