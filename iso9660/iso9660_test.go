package iso9660

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestVolumeDescriptor(t *testing.T) {
	v := open(t).Volume()
	if v.VolumeID != "TEST VOLUME" {
		t.Errorf("volume id %q", v.VolumeID)
	}
	if v.SystemID != "RIPPERX-TEST" || v.Publisher != "A PUBLISHER" ||
		v.Preparer != "A PREPARER" || v.Application != "AN APPLICATION" {
		t.Errorf("identifiers came out as %+v", v)
	}
	if v.Sectors != 24 || v.Bytes != 24*BlockSize {
		t.Errorf("size %d sectors / %d bytes", v.Sectors, v.Bytes)
	}
	if v.Joliet {
		t.Error("this volume has no Joliet tree")
	}
	if want := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC); !v.Created.Equal(want) {
		t.Errorf("created %v, want %v", v.Created, want)
	}
}

func TestReadDirPutsDirectoriesFirst(t *testing.T) {
	entries, err := open(t).ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2: %+v", len(entries), entries)
	}
	if !entries[0].IsDir || entries[0].Name != "SUB" {
		t.Errorf("first entry is %+v, want the directory", entries[0])
	}
	// The version suffix is stripped: nobody wants to see HELLO.TXT;1.
	if entries[1].Name != "HELLO.TXT" {
		t.Errorf("file is named %q", entries[1].Name)
	}
	if entries[1].Size != int64(len(helloText)) {
		t.Errorf("size %d, want %d", entries[1].Size, len(helloText))
	}
	// A directory's own extent length is not what anyone means by the size
	// of a folder.
	if entries[0].Size != 0 {
		t.Errorf("a directory reported size %d, want 0", entries[0].Size)
	}
}

func TestOpenReadsAFileFromItsExtent(t *testing.T) {
	fsys := open(t)
	r, e, err := fsys.Open("/HELLO.TXT")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != helloText {
		t.Errorf("read %q, want %q", got, helloText)
	}
	if e.Path != "/HELLO.TXT" || e.Extent != lbaFile {
		t.Errorf("entry came out as %+v", e)
	}

	// A seek is how a Range request lands in the middle of a big file.
	if _, err := r.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != helloText[7:] {
		t.Errorf("after seeking: %q", rest)
	}
}

func TestRockRidgeNameReplacesTheISOName(t *testing.T) {
	entries, err := open(t).ReadDir("/SUB")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries in /SUB", len(entries))
	}
	e := entries[0]
	if e.Name != longName {
		t.Errorf("name is %q, want the Rock Ridge name %q", e.Name, longName)
	}
	if e.ISOName != "LONG.TXT" {
		t.Errorf("the 8.3 name should be kept as well, got %q", e.ISOName)
	}
	if e.Path != "/SUB/"+longName {
		t.Errorf("path is %q", e.Path)
	}
	// And the long name has to be usable as a path, since it is the one the
	// page shows and therefore the one it asks for.
	if _, _, err := open(t).Open(e.Path); err != nil {
		t.Errorf("opening by the long name: %v", err)
	}
}

func TestWalkVisitsEverything(t *testing.T) {
	var paths []string
	if err := open(t).Walk("/", func(e Entry) error {
		paths = append(paths, e.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/SUB", "/SUB/" + longName, "/HELLO.TXT"}
	if len(paths) != len(want) {
		t.Fatalf("walked %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("walk step %d is %q, want %q", i, paths[i], want[i])
		}
	}
}

// Paths come from a request, so the walk must stay inside the volume and
// must refuse a name that is not there rather than returning something else.
func TestPathsAreCleanedAndBounded(t *testing.T) {
	fsys := open(t)
	// A path that climbs above the root is rooted again rather than
	// following the machine's own filesystem: what comes back is the
	// volume's root, never anything outside it.
	for _, p := range []string{"/SUB/../../..", "/./SUB/..", "//SUB/../"} {
		e, err := fsys.Stat(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if !e.IsDir || e.Name != "/" {
			t.Errorf("%s resolved to %+v, want the root", p, e)
		}
	}
	// The same rooting turns a traversal into a look-up of a path that
	// simply is not on this disc - which is a refusal, not an escape.
	if _, err := fsys.Stat("/../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a traversal gave %v, want ErrNotFound", err)
	}
	if _, err := fsys.Stat("/NOPE.TXT"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing file gave %v, want ErrNotFound", err)
	}
	if _, _, err := fsys.Open("/SUB"); !errors.Is(err, ErrIsDir) {
		t.Errorf("opening a directory gave %v, want ErrIsDir", err)
	}
	if _, err := fsys.ReadDir("/HELLO.TXT"); !errors.Is(err, ErrNotDir) {
		t.Errorf("listing a file gave %v, want ErrNotDir", err)
	}
	// Names are matched without regard to case: ISO 9660 names are uppercase
	// by rule, and a path typed from a Joliet listing should still find them.
	if _, err := fsys.Stat("/hello.txt"); err != nil {
		t.Errorf("a lowercase path should still find the file: %v", err)
	}
}

// A multi-session disc carries a set of volume descriptors per session, and
// only the last set describes everything on it: a later session's directory
// refers back to the files written earlier as well as to its own. Reading
// the first session's descriptors gives what was on the disc before
// anything was added - a plausible answer, and the wrong one.
func TestOpenSessionReadsTheLaterSession(t *testing.T) {
	const sessionStart = 40

	// The first session, then a second one further along the disc whose
	// root directory has a file the first does not.
	img := buildISO(t)
	// Room for the second session, which sits sessionStart sectors further
	// along and has the same layout as the first.
	img = append(img, make([]byte, 48*BlockSize)...)

	sector := func(n int) []byte { return img[n*BlockSize : (n+1)*BlockSize] }
	const (
		lbaRoot2 = sessionStart + 18
		lbaFile2 = sessionStart + 20
	)
	const added = "written later\n"

	pvd := sector(sessionStart + lbaPVD)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], pad("RIPPERX-TEST", 32))
	copy(pvd[40:72], pad("SECOND SESSION", 32))
	both32(pvd[80:88], uint32(len(img)/BlockSize))
	copy(pvd[156:190], dirRecord("\x00", lbaRoot2, BlockSize, true, nil))

	term := sector(sessionStart + lbaTerm)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	root := sector(lbaRoot2)
	off := 0
	for _, rec := range [][]byte{
		dirRecord("\x00", lbaRoot2, BlockSize, true, nil),
		dirRecord("\x01", lbaRoot2, BlockSize, true, nil),
		// Pointing back at the first session's file is what makes the old
		// data stay visible; that is the whole point of appending.
		dirRecord("HELLO.TXT;1", lbaFile, uint32(len(helloText)), false, nil),
		dirRecord("LATER.TXT;1", lbaFile2, uint32(len(added)), false, nil),
	} {
		copy(root[off:], rec)
		off += len(rec)
	}
	copy(sector(lbaFile2), added)

	// Read from the start of the disc: the first session, as it was.
	first, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Volume().VolumeID; got != "TEST VOLUME" {
		t.Errorf("the first session is %q", got)
	}
	if _, err := first.Stat("/LATER.TXT"); !errors.Is(err, ErrNotFound) {
		t.Error("the first session should not know about the later file")
	}

	// Read from the later session: both files.
	latest, err := OpenSession(bytes.NewReader(img), sessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if got := latest.Volume().VolumeID; got != "SECOND SESSION" {
		t.Errorf("the later session is %q", got)
	}
	r, _, err := latest.Open("/LATER.TXT")
	if err != nil {
		t.Fatalf("the appended file is missing: %v", err)
	}
	if got, _ := io.ReadAll(r); string(got) != added {
		t.Errorf("the appended file reads %q", got)
	}
	// And what was on the disc before is still there.
	r, _, err = latest.Open("/HELLO.TXT")
	if err != nil {
		t.Fatalf("the earlier session's file vanished: %v", err)
	}
	if got, _ := io.ReadAll(r); string(got) != helloText {
		t.Errorf("the earlier file reads %q", got)
	}
}

func TestNotAnISO(t *testing.T) {
	junk := make([]byte, 24*BlockSize)
	if _, err := Open(bytes.NewReader(junk)); !errors.Is(err, ErrNotISO9660) {
		t.Errorf("a disc with no volume descriptor gave %v, want ErrNotISO9660", err)
	}
	// An audio CD has no data at all to read: that has to be an error, not
	// a panic.
	if _, err := Open(bytes.NewReader(nil)); err == nil {
		t.Error("an empty reader must not open as a volume")
	}
}

// Joliet spells every identifier in UCS-2, which is what carries the
// characters an 8.3 name cannot.
func TestUCS2Decoding(t *testing.T) {
	ucs2 := func(s string) []byte {
		var b []byte
		for _, r := range s {
			b = append(b, byte(r>>8), byte(r))
		}
		return b
	}
	in := append(ucs2("Програ́мма"), ucs2("   ")...)
	if got := decodeText(in, true); got != "Програ́мма" {
		t.Errorf("decoded %q", got)
	}
	if got := decodeName(ucs2("Setup.exe;1"), true); got != "Setup.exe" {
		t.Errorf("decodeName gave %q", got)
	}
	// The two special names have to survive whichever tree they are in.
	if got := decodeName([]byte{0}, false); got != "." {
		t.Errorf("the self entry decoded as %q", got)
	}
	if got := decodeName([]byte{1}, false); got != ".." {
		t.Errorf("the parent entry decoded as %q", got)
	}
}

func TestIsJoliet(t *testing.T) {
	esc := make([]byte, 32)
	copy(esc, "%/E")
	if !isJoliet(esc) {
		t.Error("%/E is one of Joliet's three escape sequences")
	}
	copy(esc, "%/9")
	if isJoliet(esc) {
		t.Error("another character set is not Joliet and belongs to the primary tree")
	}
}
