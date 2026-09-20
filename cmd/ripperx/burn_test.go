package main

import (
	"bufio"
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
