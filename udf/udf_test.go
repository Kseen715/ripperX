package udf

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Kseen715/ripperX/discfs"
)

func TestVolume(t *testing.T) {
	v := openTest(t, false).Volume()
	if v.Format != "UDF" {
		t.Errorf("format = %q, want UDF", v.Format)
	}
	if v.VolumeID != "TESTDISC" {
		t.Errorf("volumeId = %q, want TESTDISC", v.VolumeID)
	}
	// The partition says how much of the disc the volume claims, which is
	// what a rip of "only the filesystem" writes.
	if v.Sectors != partAt+partLen {
		t.Errorf("sectors = %d, want %d", v.Sectors, partAt+partLen)
	}
}

func TestReadDirPutsDirectoriesFirst(t *testing.T) {
	entries, err := openTest(t, false).ReadDir("/")
	if err != nil {
		t.Fatalf("reading the root: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"sub", "HELLO.TXT", cyrillicName}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names = %v, want %v", names, want)
			break
		}
	}
	if !entries[0].IsDir {
		t.Error("the directory is not marked as one")
	}
	// The entry for the parent directory has no name and is not a file
	// anyone lists.
	for _, e := range entries {
		if e.Name == "" {
			t.Error("the parent entry was listed as a file")
		}
	}
}

func TestOpenReadsAFile(t *testing.T) {
	fs := openTest(t, false)
	r, e, err := fs.Open("/HELLO.TXT")
	if err != nil {
		t.Fatalf("opening the file: %v", err)
	}
	if e.Size != int64(len(helloText)) {
		t.Errorf("size = %d, want %d", e.Size, len(helloText))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != helloText {
		t.Errorf("contents = %q, want %q", got, helloText)
	}
}

// UDF names are Unicode, so a disc labelled in Cyrillic keeps its names.
// The file is also small enough to be stored inside its own entry, which is
// the third way contents can be recorded.
func TestACyrillicNameAndAFileInsideItsOwnEntry(t *testing.T) {
	fs := openTest(t, false)
	r, e, err := fs.Open("/" + cyrillicName)
	if err != nil {
		t.Fatalf("opening %s: %v", cyrillicName, err)
	}
	if e.Name != cyrillicName {
		t.Errorf("name = %q, want %q", e.Name, cyrillicName)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != cyrillicText {
		t.Errorf("contents = %q, want %q", got, cyrillicText)
	}
}

// Both shapes of file entry are in the wild: the plain one on every
// DVD-Video, the extended one on anything written to UDF 2.00 or later.
func TestExtendedFileEntriesReadTheSame(t *testing.T) {
	fs := openTest(t, true)
	r, e, err := fs.Open("/HELLO.TXT")
	if err != nil {
		t.Fatalf("opening the file: %v", err)
	}
	if e.Size != int64(len(helloText)) {
		t.Errorf("size = %d, want %d", e.Size, len(helloText))
	}
	got, _ := io.ReadAll(r)
	if string(got) != helloText {
		t.Errorf("contents = %q, want %q", got, helloText)
	}
	if e.ModTime.Year() != 2013 {
		t.Errorf("modTime = %s, want 2013", e.ModTime)
	}
}

func TestWalkVisitsEverything(t *testing.T) {
	var seen []string
	err := openTest(t, false).Walk("/", func(e discfs.Entry) error {
		seen = append(seen, e.Path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking: %v", err)
	}
	want := map[string]bool{
		"/sub": true, "/sub/split.bin": true,
		"/HELLO.TXT": true, "/" + cyrillicName: true,
	}
	if len(seen) != len(want) {
		t.Fatalf("walked %v, want %d entries", seen, len(want))
	}
	for _, p := range seen {
		if !want[p] {
			t.Errorf("walked %q, which is not in the volume", p)
		}
	}
}

func TestPathsAreCleanedAndBounded(t *testing.T) {
	fs := openTest(t, false)
	// A path that climbs out of the volume lands back at its root rather
	// than anywhere else.
	if _, err := fs.ReadDir("/../../.."); err != nil {
		t.Errorf("a path above the root should resolve to the root, got %v", err)
	}
	if _, _, err := fs.Open("/sub"); !errors.Is(err, ErrIsDir) {
		t.Errorf("opening a directory gave %v, want ErrIsDir", err)
	}
	if _, _, err := fs.Open("/nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("opening a missing file gave %v, want ErrNotFound", err)
	}
}

func TestNotUDF(t *testing.T) {
	// An ISO 9660 disc, or an audio CD: no anchor anywhere it must be.
	if _, err := Open(bytes.NewReader(make([]byte, volBlocks*SectorSize)), volBlocks); !errors.Is(err, ErrNotUDF) {
		t.Errorf("err = %v, want ErrNotUDF", err)
	}
	// An anchor whose checksum does not hold is a sector of rubbish that
	// happens to start with a 2, and must not be taken for one.
	img := buildVolume(t, false)
	img[256*SectorSize+4]++
	if _, err := Open(bytes.NewReader(img), volBlocks); !errors.Is(err, ErrNotUDF) {
		t.Errorf("err = %v, want ErrNotUDF for a bad checksum", err)
	}
}
