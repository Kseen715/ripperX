package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/discfs"
)

// fakeFS is a disc with named files in it and nothing else, which is all
// the title paths need: they are about which files belong together and how
// they are read as one, not about any filesystem in particular.
type fakeFS struct{ files map[string][]byte }

func (f fakeFS) Volume() discfs.Volume { return discfs.Volume{Format: "test", VolumeID: "TEST"} }

func (f fakeFS) Stat(p string) (discfs.Entry, error) {
	body, ok := f.files[p]
	if !ok {
		return discfs.Entry{}, fmt.Errorf("no such file %s", p)
	}
	return discfs.Entry{Name: path.Base(p), Path: p, Size: int64(len(body))}, nil
}

func (f fakeFS) ReadDir(dir string) ([]discfs.Entry, error) {
	var out []discfs.Entry
	for p := range f.files {
		if path.Dir(p) != dir {
			continue
		}
		e, _ := f.Stat(p)
		out = append(out, e)
	}
	if out == nil {
		return nil, fmt.Errorf("no such directory %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name }) // deliberately not sorted
	return out, nil
}

func (f fakeFS) Open(p string) (discfs.File, discfs.Entry, error) {
	e, err := f.Stat(p)
	if err != nil {
		return nil, discfs.Entry{}, err
	}
	return bytes.NewReader(f.files[p]), e, nil
}

func (f fakeFS) Walk(root string, fn func(discfs.Entry) error) error {
	for p := range f.files {
		if !strings.HasPrefix(p, root) {
			continue
		}
		e, _ := f.Stat(p)
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// dvd is a title split the way a DVD splits one: five parts, a menu VOB
// that is not part of the film, and a second title beside it.
func dvd() fakeFS {
	files := map[string][]byte{
		"/VIDEO_TS/VIDEO_TS.IFO": []byte("ifo"),
		"/VIDEO_TS/VTS_01_0.VOB": []byte("menu"),
		"/VIDEO_TS/VTS_02_1.VOB": []byte("other title"),
		"/README.TXT":            []byte("read me"),
	}
	for i := 1; i <= 5; i++ {
		files[fmt.Sprintf("/VIDEO_TS/VTS_01_%d.VOB", i)] =
			[]byte(strings.Repeat(fmt.Sprint(i), 10))
	}
	return fakeFS{files: files}
}

func TestTitlePartsGathersTheWholeFilm(t *testing.T) {
	parts, err := titleParts(dvd(), "/VIDEO_TS/VTS_01_3.VOB")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range parts {
		names = append(names, p.Name)
	}
	want := []string{"VTS_01_1.VOB", "VTS_01_2.VOB", "VTS_01_3.VOB", "VTS_01_4.VOB", "VTS_01_5.VOB"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("parts are %v, want %v", names, want)
	}
	if got := titleName(parts); got != "VTS_01.VOB" {
		t.Errorf("the title is called %q, want VTS_01.VOB", got)
	}
	if got := titleSize(parts); got != 50 {
		t.Errorf("the title is %d bytes, want 50", got)
	}
}

func TestTitlePartsLeavesEverythingElseAlone(t *testing.T) {
	for _, p := range []string{"/README.TXT", "/VIDEO_TS/VTS_01_0.VOB", "/VIDEO_TS/VTS_02_1.VOB"} {
		parts, err := titleParts(dvd(), p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(parts) != 1 || parts[0].Path != p {
			t.Errorf("%s came back as %d parts, want itself alone", p, len(parts))
		}
		if got := titleName(parts); got != path.Base(p) {
			t.Errorf("%s is called %q, want its own name", p, got)
		}
	}
}

func TestTitlePartsRefusesWhatIsNotThere(t *testing.T) {
	if _, err := titleParts(dvd(), "/VIDEO_TS/VTS_09_1.VOB"); err == nil {
		t.Fatal("a file that is not on the disc came back as a title")
	}
}

func TestMultiFileReadsThePartsAsOne(t *testing.T) {
	fsys := dvd()
	parts, err := titleParts(fsys, "/VIDEO_TS/VTS_01_1.VOB")
	if err != nil {
		t.Fatal(err)
	}
	src, err := openTitle(fsys, parts)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("1", 10) + strings.Repeat("2", 10) + strings.Repeat("3", 10) +
		strings.Repeat("4", 10) + strings.Repeat("5", 10)

	got, err := io.ReadAll(src)
	if err != nil {
		t.Fatalf("reading the title: %v", err)
	}
	if string(got) != want {
		t.Fatalf("the title read as %q, want %q", got, want)
	}

	// A read that straddles two parts is the case the format makes routine.
	buf := make([]byte, 6)
	if _, err := src.ReadAt(buf, 17); err != nil {
		t.Fatalf("reading across the boundary: %v", err)
	}
	if string(buf) != "222333" {
		t.Errorf("across the boundary read %q, want 222333", buf)
	}

	// And so is a seek towards the end, which is what a player scrubbing
	// does before it has read anything in between.
	if _, err := src.Seek(-4, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(tail) != "5555" {
		t.Errorf("the last four bytes read as %q, want 5555", tail)
	}

	if _, err := src.ReadAt(make([]byte, 4), 48); !errors.Is(err, io.EOF) {
		t.Errorf("reading past the end gave %v, want EOF", err)
	}
}

func TestOpenDVDTitleCutsTheVOBSetToTheTitle(t *testing.T) {
	// A title is a stretch of the VOB set, not a file: this one starts
	// halfway through the first VOB and ends halfway through the third.
	fsys := dvd()
	src, size, err := openDVDTitle(fsys, dvdTitle{
		Number: 1, VTS: 1,
		ranges: []byteRange{{off: 5, size: 20}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if size != 20 {
		t.Errorf("the title is %d bytes, want 20", size)
	}
	body, err := io.ReadAll(src)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("1", 5) + strings.Repeat("2", 10) + strings.Repeat("3", 5)
	if string(body) != want {
		t.Errorf("the title read as %q, want %q", body, want)
	}

	// Several cells are read as one stream, in the order they play.
	src, size, err = openDVDTitle(fsys, dvdTitle{
		Number: 2, VTS: 1,
		ranges: []byteRange{{off: 0, size: 3}, {off: 40, size: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(src)
	if size != 7 || string(body) != "1115555" {
		t.Errorf("a title of two cells is %d bytes reading %q, want 7 and 1115555", size, body)
	}
}
