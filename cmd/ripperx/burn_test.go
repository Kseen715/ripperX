package main

import (
	"bufio"
	"errors"
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/mmc"
)

// These are the checks that stand between a mistake and a disc that cannot
// be un-ruined, so each refusal is worth a test of its own.
func TestCheckImageFits(t *testing.T) {
	disc := &mmc.Disc{BlankSectors: 333000} // a 700 MB CD-R
	cases := []struct {
		name string
		size int64
		want string // a phrase the refusal must contain; empty means it must pass
	}{
		{"empty.iso", 0, "is empty"},
		{"ok.iso", 650 << 20, ""},
		{"raw.img", 2352 * 1000, "raw 2352-byte image"},
		{"odd.iso", 2048*10 + 1, "whole number of 2048-byte sectors"},
		{"huge.iso", 4 << 30, "this disc holds"},
	}
	for _, tc := range cases {
		err := checkImageFits(storedFile{Name: tc.name, Size: tc.size}, disc)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused with %v, should have been accepted", tc.name, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: accepted, should have been refused", tc.name)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: refused with %q, which does not mention %q", tc.name, err, tc.want)
		}
	}
}

// A blank disc reports its capacity in BlankSectors and nothing in Sectors;
// falling back to Sectors is what makes the check work on a disc that has
// been erased rather than one straight out of its wrapper.
func TestCheckImageFitsUsesWhicheverCapacityTheDiscGave(t *testing.T) {
	if err := checkImageFits(storedFile{Name: "x.iso", Size: 100 << 20},
		&mmc.Disc{Sectors: 333000}); err != nil {
		t.Errorf("a disc that reports only Sectors should still be measurable: %v", err)
	}
	// Neither figure known: the size check cannot be made, and refusing on a
	// guess would block a perfectly good burn.
	if err := checkImageFits(storedFile{Name: "x.iso", Size: 4 << 30}, &mmc.Disc{}); err != nil {
		t.Errorf("with no capacity known the size check must not refuse: %v", err)
	}
}

func TestBurnerArguments(t *testing.T) {
	x := &burner{path: "/usr/bin/xorriso", kind: "xorriso"}
	got := strings.Join(x.args("/dev/sr0", "/tmp/a.iso", 8, false, false), " ")
	want := "-as cdrecord -v dev=/dev/sr0 speed=8 -sao -data /tmp/a.iso"
	if got != want {
		t.Errorf("xorriso args:\n got %q\nwant %q", got, want)
	}

	c := &burner{path: "/usr/bin/cdrecord", kind: "cdrecord"}
	got = strings.Join(c.args("/dev/sr0", "/tmp/a.iso", 0, true, true), " ")
	want = "-v dev=/dev/sr0 -sao -dummy -eject -data /tmp/a.iso"
	if got != want {
		t.Errorf("cdrecord args:\n got %q\nwant %q", got, want)
	}

	if got := strings.Join(x.blankArgs("/dev/sr0", true), " "); got != "-as cdrecord -v dev=/dev/sr0 blank=all" {
		t.Errorf("blank args: %q", got)
	}
}

// The burners rewrite one line with a carriage return as they write. Split
// on either ending or the progress bar never moves until the burn is over.
func TestScanLinesOrCR(t *testing.T) {
	in := "Track 01:   1 of  250 MB written\rTrack 01:   2 of  250 MB written\nDone\n"
	sc := bufio.NewScanner(strings.NewReader(in))
	sc.Split(scanLinesOrCR)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if len(got) != 3 || got[2] != "Done" {
		t.Fatalf("got %q, want three lines ending in Done", got)
	}
	m := progressLine.FindStringSubmatch(got[1])
	if m == nil || m[1] != "2" || m[2] != "250" {
		t.Errorf("the progress line did not parse: %q -> %v", got[1], m)
	}
}

func TestFindBurnerReportsNothingWhenThereIsNothing(t *testing.T) {
	if b := findBurner("/nonexistent/burner"); b != nil {
		t.Errorf("a burner that is not installed must not be reported as one: %+v", b)
	}
}

// burnBlocker is what the page, the API and the burn job all consult, so
// each reason has to come out in words a person can act on.
func TestBurnBlocker(t *testing.T) {
	s := &server{allowBurn: true, burner: &burner{path: "xorriso", kind: "xorriso"}}
	writer := &mmc.Capabilities{Write: mmc.MediaSupport{CDR: true, CDRW: true}}

	cases := []struct {
		name string
		srv  *server
		caps *mmc.Capabilities
		disc *mmc.Disc
		want string
	}{
		{"burning turned off", &server{allowBurn: false}, writer, nil, "turned off"},
		{"no burner installed", &server{allowBurn: true}, writer, nil, "no burner program"},
		{"read-only drive", s, &mmc.Capabilities{}, nil, "only read"},
		{"empty tray", s, writer, &mmc.Disc{}, "no disc"},
		{"pressed disc", s, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileCDROM, ProfileName: "CD-ROM"}, "cannot be written"},
		{"closed CD-R", s, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileCDR, Status: mmc.DiscComplete}, "closed and cannot be erased"},
		{"used CD-RW", s, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileCDRW, Status: mmc.DiscAppendable, Erasable: true}, "erase it first"},
		{"blank CD-R", s, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileCDR, Status: mmc.DiscEmpty}, ""},
	}
	for _, tc := range cases {
		got := tc.srv.burnBlocker(tc.caps, tc.disc)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: blocked with %q, should have been allowed", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: blocked with %q, which does not mention %q", tc.name, got, tc.want)
		}
	}
}

// Burning and appending are different questions with different answers, and
// the most common disc of all - one with data on it that is not closed -
// answers no to the first and yes to the second. Answering only the first
// was wrong.
func TestAppendBlocker(t *testing.T) {
	xorriso := &server{allowBurn: true, burner: &burner{path: "xorriso", kind: "xorriso"}}
	writer := &mmc.Capabilities{Write: mmc.MediaSupport{CDR: true, DVDR: true}}
	open := &mmc.Disc{
		Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscAppendable,
		Appendable: true, WritableSectors: 170288, WritableBytes: 170288 * 2048,
	}

	cases := []struct {
		name string
		srv  *server
		caps *mmc.Capabilities
		disc *mmc.Disc
		want string
	}{
		{"an open disc with room", xorriso, writer, open, ""},
		{"burning turned off", &server{allowBurn: false}, writer, open, "turned off"},
		{"no burner", &server{allowBurn: true}, writer, open, "no burner program"},
		{"cdrecord cannot do it", &server{allowBurn: true,
			burner: &burner{path: "cdrecord", kind: "cdrecord"}}, writer, open, "needs xorriso"},
		{"read-only drive", xorriso, &mmc.Capabilities{}, open, "only read"},
		{"no disc", xorriso, writer, &mmc.Disc{}, "no disc"},
		{"a blank disc should be burned", xorriso, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscEmpty}, "burn it rather"},
		{"a closed disc", xorriso, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscComplete}, "has been closed"},
		{"an open disc with no room left", xorriso, writer, &mmc.Disc{
			Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscAppendable}, "full"},
	}
	for _, tc := range cases {
		got := tc.srv.appendBlocker(tc.caps, tc.disc)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: blocked with %q, should have been allowed", tc.name, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: blocked with %q, which does not mention %q", tc.name, got, tc.want)
		}
	}

	// And the burn refusal for that same disc now points at the alternative
	// rather than stopping at "no".
	if why := xorriso.burnBlocker(writer, open); !strings.Contains(why, "added to it") {
		t.Errorf("the burn refusal does not mention appending: %q", why)
	}
}

// The folder is handed to a burner program as a path inside the disc's own
// filesystem, so a name out of a request has no business steering it.
func TestDiscFolder(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", "/"},
		{"extras", "/extras"},
		{"/extras", "/extras"},
		{"/extras/", "/extras"},
		{"a/b", "/a/b"},
		{"/./a", "/a"},
	}
	for _, tc := range ok {
		got, err := discFolder(tc.in)
		if err != nil {
			t.Errorf("discFolder(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("discFolder(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"../etc", "/a/../../b", `a\b`, "with space", "naïve", "a/*/b"} {
		if got, err := discFolder(bad); err == nil && strings.Contains(got, "..") {
			t.Errorf("discFolder(%q) = %q, which climbs", bad, got)
		}
	}
	// Whatever comes back is absolute and never climbs, for every input.
	for _, in := range []string{"", "x", "../../etc", "/a/b/../c"} {
		got, err := discFolder(in)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(got, "/") || strings.Contains(got, "..") {
			t.Errorf("discFolder(%q) = %q", in, got)
		}
	}
}

// xorriso reports progress in its own format, and there are three
// percentages on the line. The one that matters is the first; the others
// are how full its buffers are, and matching those makes the bar jump about
// at random.
func TestXorrisoProgressLine(t *testing.T) {
	line := "xorriso : UPDATE : Writing:         16s    1.0%   fifo  72%  buf 100%    0.0xD"
	m := xorrisoPercent.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no match in %q", line)
	}
	if m[1] != "1.0" {
		t.Errorf("matched %q, want the write percentage 1.0", m[1])
	}

	later := "xorriso : UPDATE : Writing:       124s   87.5%   fifo   0%  buf  50%    2.1xD"
	if m := xorrisoPercent.FindStringSubmatch(later); m == nil || m[1] != "87.5" {
		t.Errorf("matched %v in %q, want 87.5", m, later)
	}
	// A line with no write progress on it must not match one of the buffer
	// figures instead.
	for _, other := range []string{
		"xorriso : UPDATE : fifo  72%  buf 100%",
		"xorriso : NOTE : Loading ISO image tree from LBA 0",
	} {
		if m := xorrisoPercent.FindStringSubmatch(other); m != nil {
			t.Errorf("%q matched %v, want no match", other, m)
		}
	}
}

// "The drive would not tell me how much room is left" and "there is no room
// left" are different answers, and only one of them means give up. Reporting
// the first as the second made a DVD with 300 MB free read as a full disc,
// because the free-space question had been asked about the wrong track.
func TestUnknownFreeSpaceIsNotTheSameAsFull(t *testing.T) {
	s := &server{allowBurn: true, burner: &burner{path: "xorriso", kind: "xorriso"}}
	writer := &mmc.Capabilities{Write: mmc.MediaSupport{DVDR: true}}

	full := &mmc.Disc{
		Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscAppendable,
	}
	if got := s.appendBlocker(writer, full); !strings.Contains(got, "full") {
		t.Errorf("a disc with no room says %q, want it to say full", got)
	}

	unknown := &mmc.Disc{
		Present: true, Profile: mmc.ProfileDVDRSeq, Status: mmc.DiscAppendable,
		FreeSpaceError: "READ TRACK INFORMATION: a field in the command is invalid for this drive",
	}
	got := s.appendBlocker(writer, unknown)
	if strings.Contains(got, "full") {
		t.Errorf("a disc whose free space is unknown was called full: %q", got)
	}
	if !strings.Contains(got, "would not say") {
		t.Errorf("the refusal does not say what actually happened: %q", got)
	}
	// And the reason the drive gave is passed through, because it is the
	// only thing that makes it diagnosable.
	if !strings.Contains(got, "READ TRACK INFORMATION") {
		t.Errorf("the drive's own words were dropped: %q", got)
	}
}

// The read-only library must refuse to be written to structurally, not by
// convention: a convention is one forgotten call away from deleting
// somebody's Windows ISO.
func TestReadOnlyStoreRefusesWrites(t *testing.T) {
	ro := readOnlyStore{emptyStore{}}
	if _, err := ro.Create("x.iso"); !errors.Is(err, errReadOnlyStore) {
		t.Errorf("Create gave %v, want a refusal", err)
	}
	if err := ro.Remove("x.iso"); !errors.Is(err, errReadOnlyStore) {
		t.Errorf("Remove gave %v, want a refusal", err)
	}
	if _, ok := ro.FreeBytes(); ok {
		t.Error("free space is meaningless for a library nothing is written to")
	}
	// Reading still works, which is the whole point of having it.
	if _, _, err := ro.Open("x.iso"); errors.Is(err, errReadOnlyStore) {
		t.Error("reading was refused as if it were a write")
	}
	// A read-only local store must not hand out a path the burner could
	// write through.
	if _, ok := any(ro).(interface{ localPath(string) (string, bool) }); ok {
		t.Error("the read-only wrapper exposes localPath, which reaches the directory behind it")
	}
}

// Which library a request means, and what happens when it names one that is
// not there.
func TestSourceStore(t *testing.T) {
	writable := emptyStore{}
	library := readOnlyStore{emptyStore{}}
	s := &server{store: writable, isos: library}

	for _, name := range []string{"", "images"} {
		got, err := s.sourceStore(name)
		if err != nil || got != store(writable) {
			t.Errorf("source %q gave %v, %v; want the writable store", name, got, err)
		}
	}
	if got, err := s.sourceStore("isos"); err != nil || got != store(library) {
		t.Errorf("source isos gave %v, %v", got, err)
	}
	if _, err := s.sourceStore("elsewhere"); err == nil {
		t.Error("an unknown source was accepted")
	}

	// A server with no library says so rather than falling back to the
	// writable store, which would burn the wrong file.
	none := &server{store: writable}
	if _, err := none.sourceStore("isos"); !errors.Is(err, errNoISOStore) {
		t.Errorf("with no library configured, source isos gave %v", err)
	}
}

// Saying which disc an image needs is the difference between finding out
// now and finding out with a blank in the drive.
func TestDiscNeeded(t *testing.T) {
	cases := []struct {
		size int64
		want string
	}{
		{100 << 20, "CD"},
		{capacityCD80, "CD"},
		{capacityCD80 + 1, "DVD"},
		{capacityDVD, "DVD"},
		{capacityDVD + 1, "dual-layer DVD"},
		{capacityDVDDL, "dual-layer DVD"},
		{capacityDVDDL + 1, "nothing this drive writes"},
	}
	for _, tc := range cases {
		if got := discNeeded(tc.size); got != tc.want {
			t.Errorf("discNeeded(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}
