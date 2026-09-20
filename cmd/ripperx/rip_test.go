package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

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

func TestMSF(t *testing.T) {
	cases := []struct {
		lba  int64
		want string
	}{
		{0, "00:00:00"},
		{74, "00:00:74"},
		{75, "00:01:00"},
		{75 * 60, "01:00:00"},
		{-5, "00:00:00"},
	}
	for _, tc := range cases {
		if got := msf(tc.lba); got != tc.want {
			t.Errorf("msf(%d) = %q, want %q", tc.lba, got, tc.want)
		}
	}
}

// Every writer closes the sink to learn whether the file landed, and then a
// deferred cleanup closes it again. On a local file that is harmless; on an
// SMB share the second close hangs, so a rip to a share wrote its file and
// then never finished. The sink has to tolerate it.
func TestSinkCloseIsIdempotent(t *testing.T) {
	counter := &countingCloser{}
	k := &sink{w: counter, hash: sha256.New(), rec: &jobRecord{}}

	if _, err := k.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Errorf("the second close returned %v, want nil", err)
	}
	if counter.closes != 1 {
		t.Errorf("the underlying file was closed %d times, want once", counter.closes)
	}
	// The hash is still whatever was written, closed or not.
	if k.sum() != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("hash came out as %s", k.sum())
	}
}

type countingCloser struct{ closes int }

func (c *countingCloser) Write(p []byte) (int, error) { return len(p), nil }

func (c *countingCloser) Close() error { c.closes++; return nil }

// A file in a damaged sector reads short. The tar entry's header has
// already been written with the full length, so the copy has to pad - or
// every entry after it in the archive is misaligned and lost.
func TestCopyCtxPadsAShortRead(t *testing.T) {
	var out bytes.Buffer
	n, err := copyCtx(context.Background(), &out, strings.NewReader("abc"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 || out.Len() != 10 {
		t.Fatalf("wrote %d bytes (n=%d), want 10", out.Len(), n)
	}
	if !bytes.Equal(out.Bytes(), append([]byte("abc"), make([]byte, 7)...)) {
		t.Errorf("the padding is not zeroes: %q", out.Bytes())
	}
}

// A source longer than the header promised must be cut, for the same reason.
func TestCopyCtxTruncatesALongRead(t *testing.T) {
	var out bytes.Buffer
	if _, err := copyCtx(context.Background(), &out, strings.NewReader("abcdefghij"), 4); err != nil {
		t.Fatal(err)
	}
	if out.String() != "abcd" {
		t.Errorf("wrote %q, want %q", out.String(), "abcd")
	}
}

func TestCopyCtxStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyCtx(ctx, io.Discard, strings.NewReader("abc"), 3); err == nil {
		t.Error("a cancelled copy must stop rather than finish")
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{512, "512 bytes"},
		{2048, "2.0 kB"},
		{700 << 20, "700.0 MB"},
		{5046586572, "4.7 GB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
