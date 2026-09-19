package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

// testVolume builds the smallest ISO 9660 volume that has a file in it, so
// the archive and rip paths can be exercised without a disc. It is
// deliberately minimal - a root directory and two files, no Joliet and no
// Rock Ridge; the reader itself is tested thoroughly in its own package.
func testVolume(t *testing.T) *iso9660.FS {
	t.Helper()
	const (
		lbaPVD  = 16
		lbaRoot = 18
		lbaData = 19
	)
	const body = "hello, disc\n"

	img := make([]byte, 24*iso9660.BlockSize)
	sector := func(n int) []byte { return img[n*iso9660.BlockSize : (n+1)*iso9660.BlockSize] }

	both := func(b []byte, v uint32) {
		b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		b[4], b[5], b[6], b[7] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	record := func(name string, extent, size uint32, isDir bool) []byte {
		n := 33 + len(name)
		if len(name)%2 == 0 {
			n++
		}
		rec := make([]byte, n)
		rec[0] = byte(n)
		both(rec[2:10], extent)
		both(rec[10:18], size)
		rec[18], rec[19], rec[20] = 125, 6, 15
		if isDir {
			rec[25] = 0x02
		}
		rec[28], rec[31] = 1, 1
		rec[32] = byte(len(name))
		copy(rec[33:], name)
		return rec
	}

	pvd := sector(lbaPVD)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], bytes.Repeat([]byte{' '}, 32))
	copy(pvd[40:72], append([]byte("TEST"), bytes.Repeat([]byte{' '}, 28)...))
	both(pvd[80:88], 24)
	copy(pvd[156:190], record("\x00", lbaRoot, iso9660.BlockSize, true))

	term := sector(17)
	term[0] = 255
	copy(term[1:6], "CD001")

	root := sector(lbaRoot)
	off := 0
	for _, rec := range [][]byte{
		record("\x00", lbaRoot, iso9660.BlockSize, true),
		record("\x01", lbaRoot, iso9660.BlockSize, true),
		record("A.TXT;1", lbaData, uint32(len(body)), false),
		record("B.TXT;1", lbaData, uint32(len(body)), false),
	} {
		copy(root[off:], rec)
		off += len(rec)
	}
	copy(sector(lbaData), body)

	fsys, err := iso9660.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("building the test volume: %v", err)
	}
	return fsys
}

// A menu entry that cannot be produced is worse than one that is missing,
// so every format offered has to actually build a writer.
func TestEveryOfferedFormatWorks(t *testing.T) {
	for _, f := range archiveFormats {
		var buf bytes.Buffer
		w, err := newArchiveWriter(f, &buf)
		if err != nil {
			t.Errorf("%s: %v", f.ID, err)
			continue
		}
		e := iso9660.Entry{Name: "a.txt", Path: "/dir/a.txt", Size: 5, ModTime: time.Now()}
		if err := w.addFile("dir/a.txt", e, bytes.NewReader([]byte("hello"))); err != nil {
			t.Errorf("%s: adding a file: %v", f.ID, err)
			continue
		}
		if err := w.Close(); err != nil {
			t.Errorf("%s: closing: %v", f.ID, err)
			continue
		}
		if buf.Len() == 0 {
			t.Errorf("%s: produced nothing", f.ID)
		}
		if f.Extension == "" || f.MediaType == "" || f.Note == "" {
			t.Errorf("%s: the menu entry is missing its extension, type or note", f.ID)
		}
	}
	if _, ok := archiveByID("rar"); ok {
		t.Error("rar is offered but cannot be produced")
	}
	if _, ok := archiveByID("7z"); ok {
		t.Error("7z is offered but cannot be produced")
	}
	// No format named is the same as choosing the default rather than an
	// error, because that is what an old client sends.
	if f, ok := archiveByID(""); !ok || f.ID != "zip" {
		t.Errorf("the default format is %+v, want zip", f)
	}
}

// The archives have to unpack, with the tree the disc had and the contents
// intact. Each is read back with a different decoder, which is the only way
// to know the stack was closed in the right order - a compressor closed
// after its own writer produces a file that looks fine and is truncated.
func TestArchivesUnpack(t *testing.T) {
	entries := []struct {
		path, body string
	}{
		{"/readme.txt", "hello, disc\n"},
		{"/sub/deeper.bin", "0123456789"},
	}

	build := func(t *testing.T, id string) []byte {
		t.Helper()
		f, _ := archiveByID(id)
		var buf bytes.Buffer
		w, err := newArchiveWriter(f, &buf)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			err := w.addFile(strings.TrimPrefix(e.path, "/"), iso9660.Entry{
				Name: e.path, Path: e.path, Size: int64(len(e.body)), ModTime: time.Now(),
			}, bytes.NewReader([]byte(e.body)))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	t.Run("zip", func(t *testing.T) {
		raw := build(t, "zip")
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if len(zr.File) != len(entries) {
			t.Fatalf("%d entries, want %d", len(zr.File), len(entries))
		}
		for i, f := range zr.File {
			// The leading slash is dropped, so unpacking makes the same tree.
			if want := entries[i].path[1:]; f.Name != want {
				t.Errorf("entry %d is named %q, want %q", i, f.Name, want)
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if string(got) != entries[i].body {
				t.Errorf("entry %d reads %q", i, got)
			}
		}
	})

	for _, tc := range []struct {
		id   string
		wrap func([]byte) (io.Reader, error)
	}{
		{"tar", func(b []byte) (io.Reader, error) { return bytes.NewReader(b), nil }},
		{"tar.gz", func(b []byte) (io.Reader, error) { return gzip.NewReader(bytes.NewReader(b)) }},
		{"tar.bz2", func(b []byte) (io.Reader, error) {
			return bzip2.NewReader(bytes.NewReader(b), nil)
		}},
		{"tar.xz", func(b []byte) (io.Reader, error) { return xz.NewReader(bytes.NewReader(b)) }},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r, err := tc.wrap(build(t, tc.id))
			if err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(r)
			for i, want := range entries {
				h, err := tr.Next()
				if err != nil {
					t.Fatalf("entry %d: %v", i, err)
				}
				if h.Name != want.path[1:] {
					t.Errorf("entry %d is named %q", i, h.Name)
				}
				got, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want.body {
					t.Errorf("entry %d reads %q, want %q", i, got, want.body)
				}
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Errorf("the archive has more in it than was put there: %v", err)
			}
		})
	}
}

// A file whose sectors are damaged reads short. The header has already
// promised a length, so the entry must be padded - otherwise every entry
// after it in a tar is misaligned and the whole archive is lost.
func TestShortFileDoesNotCorruptTheRest(t *testing.T) {
	f, _ := archiveByID("tar")
	var buf bytes.Buffer
	w, err := newArchiveWriter(f, &buf)
	if err != nil {
		t.Fatal(err)
	}
	// Claims 20 bytes, supplies 3.
	err = w.addFile("bad", iso9660.Entry{Name: "bad", Path: "/bad", Size: 20}, bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatal(err)
	}
	err = w.addFile("good", iso9660.Entry{Name: "good", Path: "/good", Size: 4}, bytes.NewReader([]byte("fine")))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	first, _ := io.ReadAll(tr)
	if len(first) != 20 {
		t.Errorf("the damaged entry is %d bytes, want it padded to the 20 it promised", len(first))
	}
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("the entry after a damaged one was lost: %v", err)
	}
	if h.Name != "good" {
		t.Errorf("the second entry is %q", h.Name)
	}
	rest, _ := io.ReadAll(tr)
	if string(rest) != "fine" {
		t.Errorf("the second entry reads %q", rest)
	}
}

func TestSymlinksSurvive(t *testing.T) {
	e := iso9660.Entry{Name: "link", Path: "/link", SymlinkTarget: "../target"}
	f, _ := archiveByID("tar")
	var buf bytes.Buffer
	w, _ := newArchiveWriter(f, &buf)
	if err := w.addSymlink("link", e); err != nil {
		t.Fatal(err)
	}
	w.Close()

	h, err := tar.NewReader(bytes.NewReader(buf.Bytes())).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Typeflag != tar.TypeSymlink || h.Linkname != "../target" {
		t.Errorf("the symlink came out as %+v", h)
	}
}

// Deflating a JPEG costs the whole compression pass and saves nothing, so
// those are stored instead.
func TestAlreadyCompressedFilesAreStored(t *testing.T) {
	for _, name := range []string{"photo.JPG", "archive.zip", "song.flac", "disc.iso"} {
		if !alreadyCompressed(name) {
			t.Errorf("%s should be stored rather than deflated", name)
		}
	}
	for _, name := range []string{"readme.txt", "setup.exe", "data.bin", "noext"} {
		if alreadyCompressed(name) {
			t.Errorf("%s is worth compressing", name)
		}
	}
}

// writeArchive reports progress in source bytes, because a compressed
// archive has no length until it is finished and a bar that cannot reach
// its end is worse than no bar.
func TestArchiveProgressCountsSourceBytes(t *testing.T) {
	fsys := testVolume(t)
	plan, err := planFiles(fsys, []string{"/"})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := archiveByID("tar.gz")
	var started int
	var lastDone int64
	err = writeArchive(context.Background(), io.Discard, f, fsys, plan, archiveProgress{
		starting: func(iso9660.Entry) { started++ },
		finished: func(done int64) {
			if done < lastDone {
				t.Errorf("progress went backwards: %d after %d", done, lastDone)
			}
			lastDone = done
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if started != len(plan.files) {
		t.Errorf("%d entries started, want %d", started, len(plan.files))
	}
	if lastDone != plan.bytes {
		t.Errorf("progress ended at %d, want the plan's %d", lastDone, plan.bytes)
	}
}

// What a rip is called, and what the paths inside it look like. Both were
// wrong in the same way: they answered with the whole disc when the
// question was about one folder on it.
func TestAnArchiveIsRootedAtWhatWasChosen(t *testing.T) {
	cases := []struct {
		what  string
		paths []string
		root  string
		name  string
	}{
		{"one folder deep in the tree", []string{"/Drivers/Win7"}, "Drivers", "Win7"},
		{"a folder at the top", []string{"/VIDEO_TS"}, "", "VIDEO_TS"},
		{"the whole disc", []string{"/"}, "", ""},
		{"one file", []string{"/Drivers/Win7/setup.exe"}, "Drivers/Win7", "setup.exe"},
		{"several files sharing a folder",
			[]string{"/Drivers/Win7/a.inf", "/Drivers/Win7/b.inf"}, "Drivers/Win7", ""},
		{"several folders that do not",
			[]string{"/Drivers/Win7", "/Docs/readme"}, "", ""},
	}
	for _, c := range cases {
		if got := commonParent(c.paths); got != c.root {
			t.Errorf("%s: the part to trim is %q, want %q", c.what, got, c.root)
		}
		if got := suggestFrom(c.paths); got != c.name {
			t.Errorf("%s: the name it suggests is %q, want %q", c.what, got, c.name)
		}
	}

	// And what that means for the entries themselves: choosing
	// /Drivers/Win7 puts Win7 at the root of the archive rather than a
	// Drivers folder holding a Win7 folder holding the files.
	plan := filePlan{root: commonParent([]string{"/Drivers/Win7"})}
	got := plan.under(iso9660.Entry{Path: "/Drivers/Win7/net/e1000.sys"})
	if got != "Win7/net/e1000.sys" {
		t.Errorf("the entry is called %q inside the archive", got)
	}
}

// A name that was typed is the name. A name that was invented carries the
// date, so two rips of the same disc do not collide.
func TestAChosenNameIsUsedAsItIs(t *testing.T) {
	s := &server{store: emptyStore{}}
	disc := &mmc.Disc{ProfileName: "DVD-ROM"}

	name, chosen := s.ripName(&drive{}, disc, "movies", "VIDEO_TS")
	if name != "movies" || !chosen {
		t.Errorf("a typed name came out as %q (chosen %v), want it used as it is", name, chosen)
	}
	name, chosen = s.ripName(&drive{}, disc, "", "VIDEO_TS")
	if chosen {
		t.Error("an invented name was reported as chosen")
	}
	if !strings.HasPrefix(name, "VIDEO_TS-") || len(name) != len("VIDEO_TS-20260919-211247") {
		t.Errorf("an invented name is %q, want the folder and the date", name)
	}
	// Nothing to go on: the disc itself.
	name, _ = s.ripName(&drive{}, disc, "", "")
	if !strings.HasPrefix(name, "DVD-ROM-") {
		t.Errorf("with nothing chosen the name is %q, want the disc's", name)
	}
}

// Two archive suffixes are one suffix, or "disc.tar.gz" twice gives
// "disc.tar-2.gz".
func TestSplitExtensionKeepsTwoPartSuffixes(t *testing.T) {
	cases := map[string][2]string{
		"disc.tar.gz": {"disc", ".tar.gz"},
		"disc.zip":    {"disc", ".zip"},
		"disc.iso":    {"disc", ".iso"},
		"disc":        {"disc", ""},
		"a.b.iso":     {"a.b", ".iso"},
	}
	for in, want := range cases {
		stem, ext := splitExtension(in)
		if stem != want[0] || ext != want[1] {
			t.Errorf("splitExtension(%q) = %q, %q, want %q, %q", in, stem, ext, want[0], want[1])
		}
	}
}
