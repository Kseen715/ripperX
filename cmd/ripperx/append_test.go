package main

import (
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/mmc"
)

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
