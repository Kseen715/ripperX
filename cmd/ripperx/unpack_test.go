package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What goes in the test archives: a file in the root, one in a directory, a
// symbolic link, and a name that is not ASCII - because a disc ripped from a
// Russian DVD is exactly what comes back through here.
var unpackFiles = []struct{ name, body string }{
	{"autorun.inf", "[autorun]\r\nopen=setup.exe\r\n"},
	{"VIDEO_TS/VTS_01_1.VOB", strings.Repeat("v", 5000)},
	{"Наклейка.txt", "надпись\n"},
}

func zipArchiveBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range unpackFiles {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink, which is what a disc mastered on Unix carries and what
	// ripperX's own tar and zip writers put in.
	h := &zip.FileHeader{Name: "latest.vob"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("VIDEO_TS/VTS_01_1.VOB")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzBytes(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if len(entries) == 0 {
		for _, f := range unpackFiles {
			must(t, tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.body)),
			}))
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatal(err)
			}
		}
		must(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink, Name: "latest.vob",
			Linkname: "VIDEO_TS/VTS_01_1.VOB", Mode: 0o777,
		}))
	}
	for i := range entries {
		must(t, tw.WriteHeader(&entries[i]))
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return buf.Bytes()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(p, body, 0o644))
	return p
}

// The round trip is what this is for: an archive ripperX wrote, unpacked
// into the tree that goes onto a disc.
func TestUnpackRestoresWhatWasRipped(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"disc.zip", zipArchiveBytes(t)},
		{"disc.tar.gz", tarGzBytes(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := writeTemp(t, tc.name, tc.body)
			dest := t.TempDir()
			files, total, err := unpackArchive(context.Background(), nil, tc.name, src, dest)
			if err != nil {
				t.Fatalf("unpacking: %v", err)
			}
			if len(files) != len(unpackFiles) {
				t.Fatalf("got %d files, want %d", len(files), len(unpackFiles))
			}
			var want int64
			for _, f := range unpackFiles {
				want += int64(len(f.body))
				got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(f.name)))
				if err != nil {
					t.Fatalf("%s: %v", f.name, err)
				}
				if string(got) != f.body {
					t.Errorf("%s came out as %q", f.name, got)
				}
			}
			if total != want {
				t.Errorf("total = %d, want %d", total, want)
			}
			// The link is a link, pointing where it pointed.
			target, err := os.Readlink(filepath.Join(dest, "latest.vob"))
			if err != nil {
				t.Fatalf("the symlink did not come out: %v", err)
			}
			if target != "VIDEO_TS/VTS_01_1.VOB" {
				t.Errorf("the symlink points at %q", target)
			}
			// Every hash is taken as the file is written, so verifying the
			// disc afterwards costs one read of the disc and nothing else.
			for _, f := range files {
				if len(f.sum) != 64 {
					t.Errorf("%s has no hash", f.rel)
				}
			}
		})
	}
}

// An archive is a file that came from somewhere else, and "../../etc/passwd"
// is a real entry in real archives.
func TestUnpackRefusesToEscape(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "/etc/passwd", `..\escaped.txt`, "a/../../b.txt"} {
		body := tarGzBytes(t, tar.Header{
			Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: 0,
		})
		src := writeTemp(t, "evil.tar.gz", body)
		dest := t.TempDir()
		_, _, err := unpackArchive(context.Background(), nil, "evil.tar.gz", src, dest)
		if err == nil {
			t.Errorf("%q was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "outside") {
			t.Errorf("%q gave %v, want it refused for leaving the root", name, err)
		}
	}
}

// A symlink is the other way out: the file stays inside, and what it points
// at does not. A link that uses ".." and stays on the disc is ordinary and
// must still work, so this is counted rather than banned.
func TestLinksMayClimbButNotEscape(t *testing.T) {
	inside := []struct{ at, target string }{
		{"VIDEO_TS/latest.vob", "../AUDIO_TS/track.aob"},
		{"a/b/c.txt", "../../d.txt"},
		{"x.txt", "y.txt"},
		{"a/b.txt", "./c.txt"},
	}
	for _, c := range inside {
		if !linkStaysInside(c.at, c.target) {
			t.Errorf("%s -> %s was refused, but stays on the disc", c.at, c.target)
		}
	}
	outside := []struct{ at, target string }{
		{"key", "../../../etc/shadow"},
		{"key", "/etc/shadow"},
		{"a/b.txt", "../../elsewhere"},
		{"x", ""},
	}
	for _, c := range outside {
		if linkStaysInside(c.at, c.target) {
			t.Errorf("%s -> %s was accepted, but leaves the disc", c.at, c.target)
		}
	}

	// And the whole way through, on a real archive.
	body := tarGzBytes(t, tar.Header{
		Typeflag: tar.TypeSymlink, Name: "key", Linkname: "../../../etc/shadow", Mode: 0o777,
	})
	src := writeTemp(t, "evil.tar.gz", body)
	_, _, err := unpackArchive(context.Background(), nil, "evil.tar.gz", src, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want it refused", err)
	}
}

func TestEmptyArchiveIsNotADisc(t *testing.T) {
	src := writeTemp(t, "empty.tar.gz", tarGzBytes(t, tar.Header{
		Typeflag: tar.TypeDir, Name: "nothing/", Mode: 0o755,
	}))
	_, _, err := unpackArchive(context.Background(), nil, "empty.tar.gz", src, t.TempDir())
	if err != errEmptyArchive {
		t.Fatalf("err = %v, want errEmptyArchive", err)
	}
}

func TestWhatCountsAsAnArchive(t *testing.T) {
	yes := []string{"disc.zip", "disc.tar", "disc.tar.gz", "disc.tgz",
		"disc.tar.xz", "disc.tar.bz2", "DISC.TAR.GZ", "a b c.zip"}
	no := []string{"disc.iso", "disc.img", "disc.rar", "disc.7z", "disc", "disc.gz"}
	for _, n := range yes {
		if !isArchive(n) {
			t.Errorf("%q is not recognised as an archive", n)
		}
	}
	for _, n := range no {
		if isArchive(n) {
			t.Errorf("%q was taken for an archive", n)
		}
	}
	// The longest suffix wins, or a .tar.gz would be opened as a .tar.
	if i, _ := archiveOf("disc.tar.gz"); archiveExtensions[i].suffix != ".tar.gz" {
		t.Errorf("disc.tar.gz opened as %q", archiveExtensions[i].suffix)
	}
}
