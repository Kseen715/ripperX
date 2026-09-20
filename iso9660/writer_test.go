package iso9660

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"
)

// An image is only worth writing if this package can read it back, so the
// test is the round trip: plan it, write it, open it, and compare what comes
// out with what went in.

type built struct {
	fs    *FS
	image []byte
}

func build(t *testing.T, items []Item, content map[string]string, opt Options) built {
	t.Helper()
	layout, err := Plan(items, opt)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	var buf bytes.Buffer
	err = layout.Write(context.Background(), &buf, func(it Item) (io.Reader, error) {
		body, ok := content[it.Path]
		if !ok {
			return nil, fmt.Errorf("no content for %s", it.Path)
		}
		return strings.NewReader(body), nil
	})
	if err != nil {
		t.Fatalf("writing: %v", err)
	}
	if int64(buf.Len()) != layout.Size() {
		t.Fatalf("the image is %d bytes and the plan promised %d", buf.Len(), layout.Size())
	}
	if buf.Len()%BlockSize != 0 {
		t.Fatalf("the image is %d bytes, which is not whole sectors", buf.Len())
	}
	fs, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("reading the image back: %v", err)
	}
	return built{fs: fs, image: buf.Bytes()}
}

func TestWrittenImageReadsBack(t *testing.T) {
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	items := []Item{
		{Path: "readme first.txt", Size: 11, ModTime: when, Mode: 0o100644},
		{Path: "Drivers", IsDir: true, ModTime: when},
		{Path: "Drivers/net card.sys", Size: 5, ModTime: when},
		{Path: "Drivers/deep/deeper/x.bin", Size: 3, ModTime: when},
		{Path: "empty.dat", Size: 0, ModTime: when},
		{Path: "link", Symlink: "Drivers/net card.sys", ModTime: when},
	}
	content := map[string]string{
		"readme first.txt":          "hello disc!",
		"Drivers/net card.sys":      "12345",
		"Drivers/deep/deeper/x.bin": "abc",
	}
	b := build(t, items, content, Options{VolumeID: "TEST_DISC", Publisher: "ripperX", Created: when})

	vol := b.fs.Volume()
	if vol.VolumeID != "TEST_DISC" {
		t.Errorf("the volume calls itself %q", vol.VolumeID)
	}
	if !vol.Joliet {
		t.Error("the image has no Joliet tree, so its long names are only 8.3")
	}
	// Open prefers the Joliet tree, which by design carries no Rock Ridge.
	// The annotations are on the primary tree, so that is where they are
	// checked from.
	primary := openPrimary(t, b.image)
	if !primary.Volume().RockRidge {
		t.Error("the primary tree does not declare Rock Ridge")
	}

	// Long names survive, which is the whole point of writing two trees.
	root, err := b.fs.ReadDir("/")
	if err != nil {
		t.Fatalf("listing the root: %v", err)
	}
	var names []string
	for _, e := range root {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	want := []string{"Drivers", "empty.dat", "link", "readme first.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("the root holds %v, want %v", names, want)
	}

	// Contents come back byte for byte.
	for path, body := range content {
		f, e, err := b.fs.Open("/" + path)
		if err != nil {
			t.Fatalf("opening /%s: %v", path, err)
		}
		if e.Size != int64(len(body)) {
			t.Errorf("/%s is %d bytes, want %d", path, e.Size, len(body))
		}
		got, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("reading /%s: %v", path, err)
		}
		if string(got) != body {
			t.Errorf("/%s reads back as %q, want %q", path, got, body)
		}
	}

	// A directory four levels down is reached by name.
	if _, err := b.fs.Stat("/Drivers/deep/deeper/x.bin"); err != nil {
		t.Errorf("the deep file is not there: %v", err)
	}

	// Rock Ridge carried the long name, the mode and the symlink.
	e, err := primary.Stat("/link")
	if err != nil {
		t.Fatalf("the symlink is missing: %v", err)
	}
	if e.SymlinkTarget != "Drivers/net card.sys" {
		t.Errorf("the symlink points at %q", e.SymlinkTarget)
	}
	e, err = primary.Stat("/readme first.txt")
	if err != nil {
		t.Fatalf("the Rock Ridge name did not survive: %v", err)
	}
	if e.Mode&0o777 != 0o644 {
		t.Errorf("the mode came back as %o, want 644", e.Mode&0o777)
	}
	if !e.ModTime.Equal(when) {
		t.Errorf("the timestamp came back as %s, want %s", e.ModTime, when)
	}
}

// Two names that flatten to the same 8.3 identifier must not become one
// file. Silently writing one over the other is the one outcome that loses
// data without saying so.
// openPrimary reads the image's primary tree rather than its Joliet one,
// which is where Rock Ridge lives. Open picks Joliet when a volume has both.
func openPrimary(t *testing.T, image []byte) *FS {
	t.Helper()
	r := bytes.NewReader(image)
	pvd := make([]byte, BlockSize)
	if _, err := r.ReadAt(pvd, systemArea*BlockSize); err != nil {
		t.Fatalf("reading the primary descriptor: %v", err)
	}
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatalf("sector %d is not a primary volume descriptor", systemArea)
	}
	d := parseDescriptor(pvd)
	fs := &FS{r: r, vol: d.volume(), root: d.root}
	fs.vol.Format = "ISO 9660"
	fs.vol.RockRidge = fs.detectRockRidge()
	return fs
}

func TestClashingNamesStayTwoFiles(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	items := []Item{
		{Path: "Photo (1).jpg", Size: 1, ModTime: when},
		{Path: "Photo [1].jpg", Size: 1, ModTime: when},
		{Path: "Photo {1}.jpg", Size: 1, ModTime: when},
	}
	content := map[string]string{
		"Photo (1).jpg": "a", "Photo [1].jpg": "b", "Photo {1}.jpg": "c",
	}
	b := build(t, items, content, Options{VolumeID: "CLASH"})

	entries, err := b.fs.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("the root holds %d entries, want 3", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name] {
			t.Errorf("%q appears twice", e.Name)
		}
		seen[e.Name] = true
		f, _, err := b.fs.Open(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(f)
		if string(got) != content[e.Name] {
			t.Errorf("%s reads back as %q, want %q", e.Name, got, content[e.Name])
		}
	}
}

// The plan has to be right about the size before anything is written: a rip
// checks it against the free space and a download declares it as a length.
func TestPlanKnowsTheSizeInAdvance(t *testing.T) {
	items := []Item{
		{Path: "a.bin", Size: 3000},
		{Path: "b/c.bin", Size: 1},
	}
	layout, err := Plan(items, Options{VolumeID: "SIZE"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = layout.Write(context.Background(), &buf, func(it Item) (io.Reader, error) {
		return strings.NewReader(strings.Repeat("x", int(it.Size))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) != layout.Size() {
		t.Errorf("the plan said %d bytes and the image is %d", layout.Size(), buf.Len())
	}
	if got := layout.Files(); len(got) != 2 {
		t.Errorf("the plan asks for %d files, want 2", len(got))
	}
}

// A file whose sectors are damaged reads short. The image must still have
// the length its directory records promise, or everything after it moves.
func TestAShortFileIsPaddedRatherThanShifting(t *testing.T) {
	items := []Item{
		{Path: "broken.bin", Size: 4096},
		{Path: "after.txt", Size: 5},
	}
	layout, err := Plan(items, Options{VolumeID: "SHORT"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = layout.Write(context.Background(), &buf, func(it Item) (io.Reader, error) {
		if it.Path == "broken.bin" {
			return strings.NewReader("only a few bytes"), nil
		}
		return strings.NewReader("after"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	f, e, err := fs.Open("/after.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 5 {
		t.Errorf("after.txt is %d bytes", e.Size)
	}
	got, _ := io.ReadAll(f)
	if string(got) != "after" {
		t.Errorf("the file after the damaged one reads as %q, so the extents moved", got)
	}
}
