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
	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

// A menu entry that cannot be produced is worse than one that is missing,
// so every format offered has to come out of the same call the page makes.
// An .iso does not go through newArchiveWriter at all - it has its own
// writer - which is exactly why this goes through writeArchive instead.
func TestEveryOfferedFormatWorks(t *testing.T) {
	fsys := testVolume(t)
	plan, err := planFiles(fsys, []string{"/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range archiveFormats {
		var buf bytes.Buffer
		err := writeArchive(context.Background(), &buf, f, fsys, plan, "disc"+f.Extension, archiveProgress{})
		if err != nil {
			t.Errorf("%s: %v", f.ID, err)
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
	err = writeArchive(context.Background(), io.Discard, f, fsys, plan, "disc.tar.gz", archiveProgress{
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

// A folder taken off a disc as an .iso has to come back as a disc: the same
// files, readable by the same reader, and burnable without being unpacked
// first. That last part is what the sector count checks - a burn refuses a
// file that is not a whole number of 2048-byte sectors.
func TestISOFormatProducesABurnableImage(t *testing.T) {
	fsys := testVolume(t)
	plan, err := planFiles(fsys, []string{"/"})
	if err != nil {
		t.Fatal(err)
	}
	format, ok := archiveByID("iso")
	if !ok {
		t.Fatal("the iso format is not offered")
	}
	var buf bytes.Buffer
	if err := writeArchive(context.Background(), &buf, format, fsys, plan, "taken.iso", archiveProgress{}); err != nil {
		t.Fatalf("writing the image: %v", err)
	}
	if buf.Len()%iso9660.BlockSize != 0 {
		t.Fatalf("the image is %d bytes, which is not whole sectors, so a burn would refuse it", buf.Len())
	}
	// The plan's own figure is what the room check and the Content-Length
	// are taken from, so it has to be the real one.
	layout, err := isoLayout(plan, "taken.iso")
	if err != nil {
		t.Fatal(err)
	}
	if layout.Size() != int64(buf.Len()) {
		t.Errorf("the plan promised %d bytes and the image is %d", layout.Size(), buf.Len())
	}

	out, err := iso9660.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("the image does not read back: %v", err)
	}
	if out.Volume().VolumeID != "TAKEN" {
		t.Errorf("the volume calls itself %q, want TAKEN", out.Volume().VolumeID)
	}
	for _, e := range plan.files {
		name := plan.under(e)
		f, got, err := out.Open("/" + name)
		if err != nil {
			t.Errorf("/%s is not in the image: %v", name, err)
			continue
		}
		if got.Size != e.Size {
			t.Errorf("/%s is %d bytes in the image and %d on the disc", name, got.Size, e.Size)
		}
		want, _, err := fsys.Open(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := io.ReadAll(f)
		b, _ := io.ReadAll(want)
		if !bytes.Equal(a, b) {
			t.Errorf("/%s came out as %q, want %q", name, a, b)
		}
	}
}
