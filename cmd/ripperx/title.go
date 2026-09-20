package main

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/Kseen715/ripperX/discfs"
)

// A DVD-Video film is not one file. The format caps a VOB at a gigabyte, so
// a two-hour title arrives as VTS_01_1.VOB through VTS_01_5.VOB, split at a
// cell boundary and meant to be read as one continuous program stream - the
// parts concatenate byte for byte, timestamps and all.
//
// Playing one of those files on its own gives ten minutes of the film and a
// scrub bar to match, which is not what anybody clicking on it wants. So a
// title is opened as one file here: the parts are found, opened, and
// presented as a single seekable stream. Nothing above this has to know
// that a DVD splits anything.

// vobPart matches a title's own VOBs. The set number groups them and the
// part number orders them; VTS_01_0.VOB is the menu for that set rather
// than part of the film, which is why part 0 is excluded.
var vobPart = regexp.MustCompile(`(?i)^VTS_(\d{2})_(\d)\.VOB$`)

// titleParts is every file that belongs with the one asked for, in the
// order they are read. Anything that is not a DVD title part is a title of
// one file, so callers need no special case for an ordinary video.
func titleParts(fsys discfs.FS, p string) ([]discfs.Entry, error) {
	self, err := fsys.Stat(p)
	if err != nil {
		return nil, err
	}
	if self.IsDir {
		return nil, fmt.Errorf("%s is a directory", p)
	}
	m := vobPart.FindStringSubmatch(self.Name)
	if m == nil || m[2] == "0" {
		return []discfs.Entry{self}, nil
	}

	siblings, err := fsys.ReadDir(path.Dir(self.Path))
	if err != nil {
		// The file itself was readable; a directory that will not list is a
		// reason to play the one part rather than to fail.
		return []discfs.Entry{self}, nil
	}
	type numbered struct {
		part  int
		entry discfs.Entry
	}
	var found []numbered
	for _, e := range siblings {
		if e.IsDir {
			continue
		}
		sm := vobPart.FindStringSubmatch(e.Name)
		if sm == nil || sm[1] != m[1] || sm[2] == "0" {
			continue
		}
		n, err := strconv.Atoi(sm[2])
		if err != nil {
			continue
		}
		found = append(found, numbered{part: n, entry: e})
	}
	if len(found) == 0 {
		return []discfs.Entry{self}, nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].part < found[j].part })
	parts := make([]discfs.Entry, 0, len(found))
	for _, f := range found {
		parts = append(parts, f.entry)
	}
	return parts, nil
}

// titleName is what a title of several parts is called: the set's name
// without the part number, so VTS_01_1.VOB and its four siblings download
// as VTS_01.VOB rather than as the name of whichever part was clicked.
func titleName(parts []discfs.Entry) string {
	if len(parts) == 1 {
		return parts[0].Name
	}
	m := vobPart.FindStringSubmatch(parts[0].Name)
	if m == nil {
		return parts[0].Name
	}
	return fmt.Sprintf("VTS_%s.VOB", m[1])
}

// titleSize is how long the whole thing is.
func titleSize(parts []discfs.Entry) int64 {
	var n int64
	for _, e := range parts {
		n += e.Size
	}
	return n
}

// openTitle opens every part and presents them as one file.
func openTitle(fsys discfs.FS, parts []discfs.Entry) (discfs.File, error) {
	if len(parts) == 0 {
		return nil, errors.New("a title with no parts")
	}
	if len(parts) == 1 {
		f, _, err := fsys.Open(parts[0].Path)
		return f, err
	}
	files := make([]discfs.File, 0, len(parts))
	sizes := make([]int64, 0, len(parts))
	for _, e := range parts {
		f, entry, err := fsys.Open(e.Path)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		sizes = append(sizes, entry.Size)
	}
	return newMultiFile(files, sizes), nil
}

// newMultiFile stitches readers together in the order given. The pieces can
// be files, as a DVD title's VOBs are, or sections of one file, as a title
// within those VOBs is.
func newMultiFile(files []discfs.File, sizes []int64) *multiFile {
	mf := &multiFile{files: files, sizes: sizes, starts: make([]int64, 0, len(files))}
	for _, n := range sizes {
		mf.starts = append(mf.starts, mf.size)
		mf.size += n
	}
	return mf
}

// multiFile is several files read as one. Only the offsets are held here;
// every read goes to the part it lands in, so a seek into the middle of the
// fourth VOB is a seek of the laser and nothing more.
type multiFile struct {
	files  []discfs.File
	starts []int64 // where each part begins in the whole
	sizes  []int64
	size   int64
	pos    int64
}

func (m *multiFile) Size() int64 { return m.size }

// part is the index of the part holding off, and the offset within it.
func (m *multiFile) part(off int64) (int, int64) {
	i := sort.Search(len(m.starts), func(i int) bool { return m.starts[i] > off }) - 1
	if i < 0 {
		return 0, off
	}
	return i, off - m.starts[i]
}

func (m *multiFile) Read(p []byte) (int, error) {
	if m.pos >= m.size {
		return 0, io.EOF
	}
	n, err := m.ReadAt(p[:m.clamp(len(p))], m.pos)
	m.pos += int64(n)
	return n, err
}

// clamp keeps a read inside one part, so a caller asking for more than the
// part holds gets a short read rather than this having to stitch buffers.
func (m *multiFile) clamp(want int) int {
	i, off := m.part(m.pos)
	if rest := m.sizes[i] - off; rest < int64(want) {
		return int(rest)
	}
	return want
}

func (m *multiFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	read := 0
	for read < len(p) {
		if off+int64(read) >= m.size {
			return read, io.EOF
		}
		i, within := m.part(off + int64(read))
		want := int64(len(p) - read)
		if rest := m.sizes[i] - within; rest < want {
			want = rest
		}
		n, err := m.files[i].ReadAt(p[read:read+int(want)], within)
		read += n
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return read, err
		}
	}
	return read, nil
}

func (m *multiFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = m.size + off
	default:
		return 0, errors.New("bad whence")
	}
	if m.pos < 0 {
		m.pos = 0
	}
	return m.pos, nil
}
