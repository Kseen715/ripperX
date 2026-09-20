package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// validName is the only thing between a name from a request and a path
// joined onto the image directory or onto an SMB share. The traversals that
// matter are the ones spelled with the separator of the *other* system:
// filepath.Base on Linux passes a backslash straight through.
func TestValidNameRefusesEscapes(t *testing.T) {
	bad := []struct{ name, why string }{
		{"", "empty"},
		{".", "the current directory"},
		{"..", "the parent directory"},
		{"../secret.iso", "a traversal"},
		{`..\..\secret.iso`, "a traversal spelled for Windows"},
		{"/etc/passwd", "an absolute path"},
		{`dir\file.iso`, "a Windows separator"},
		{"dir/file.iso", "a Unix separator"},
		{".hidden", "a hidden file"},
		{"stream.iso:$DATA", "an alternate data stream"},
		{"nul\x00.iso", "a NUL"},
		{"bell\x07.iso", "a control character"},
		{"tab\there.iso", "a tab"},
		{strings.Repeat("a", 256), "longer than any filesystem allows"},
	}
	for _, tc := range bad {
		if validName(tc.name) {
			t.Errorf("validName(%q) is true; it is %s", tc.name, tc.why)
		}
	}
}

// And what it must accept: the names real files actually have. Refusing to
// list a file is not a security property - it is a disc image nobody can
// burn. A shelf of installer images includes "tiny11 23H2 x64.iso".
func TestValidNameAcceptsRealFileNames(t *testing.T) {
	good := []string{
		"disc.iso",
		"a",
		"EPSON-20260919-093233.img",
		"x_y-z+1.tar",
		"tiny11 23H2 x64.iso",
		"ru-ru_windows_11_consumer_editions_version_24h2.iso",
		"Полис.iso",
		"naïve.iso",
		"image (copy).iso",
		"a.b.c",
		strings.Repeat("a", 255),
	}
	for _, name := range good {
		if !validName(name) {
			t.Errorf("validName(%q) is false; it is a name a real file has", name)
		}
	}
}

// Whatever validName accepts has to be safe to join onto either kind of
// path. This is the property the whole check exists for, stated directly.
func TestAcceptedNamesCannotEscapeEitherKindOfPath(t *testing.T) {
	for _, name := range []string{
		"disc.iso", "tiny11 23H2 x64.iso", "Полис.iso", "a.b.c",
		"image (copy).iso", strings.Repeat("z", 255),
	} {
		if !validName(name) {
			continue
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("%q was accepted and contains a path separator", name)
		}
		if got := filepath.Join("/images", name); filepath.Dir(got) != "/images" {
			t.Errorf("%q joins to %q, which is outside the directory", name, got)
		}
		// The same, spelled the way an SMB path is.
		smb := `share\` + name
		if strings.Count(smb, `\`) != 1 {
			t.Errorf("%q adds a separator to an SMB path: %q", name, smb)
		}
	}
}

// safeName is applied to every name ripperX invents from a disc and to every
// name a client supplies, so whatever it returns has to be a name validName
// accepts - including for input that is entirely unusable.
func TestSafeNameAlwaysProducesAValidName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"EPSON", "EPSON"},
		{"Windows 98  SE", "Windows-98-SE"},
		{"../../etc/passwd", "etc-passwd"},
		{`C:\images\disc.iso`, "C-images-disc.iso"},
		{"  ..spaces..  ", "spaces"},
		// A disc labelled in Cyrillic rips to a file named in Cyrillic.
		// The share keeps names as UTF-16 and the page sends them as
		// UTF-8; the only thing that ever lost them was this function.
		{"Наклейка", "Наклейка"},
		{"Диск 2 - фильмы", "Диск-2-фильмы"},
		{"", "fallback"},
		{"...", "fallback"},
		{strings.Repeat("x", 300), strings.Repeat("x", 180)},
		// Truncation cuts on a rune boundary: half of a two-byte letter
		// is not a name, and would not survive a round trip.
		{strings.Repeat("я", 300), strings.Repeat("я", 90)},
	}
	for _, tc := range cases {
		got := safeName(tc.in, "fallback")
		if got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !validName(got) {
			t.Errorf("safeName(%q) = %q, which validName refuses", tc.in, got)
		}
	}
}

// SMB reports the room on a share in allocation units, and an allocation
// unit is two numbers multiplied together. Using one of them reports a
// share as a whole multiple smaller than it is - which is not an error
// anything catches, because the answer looks perfectly reasonable.
func TestAllocationUnitIsBothNumbers(t *testing.T) {
	cases := []struct {
		bytesPerSector, sectorsPerUnit uint64
		want                           int64
	}{
		{512, 8, 4096},  // the ordinary NTFS cluster
		{2048, 2, 4096}, // what the share in front of me reports
		{4096, 1, 4096}, // sectors the size of the unit
		{512, 0, 512},   // a server that will not say
		{0, 8, 0},       // and one that says nothing usable
	}
	for _, c := range cases {
		if got := allocationUnit(c.bytesPerSector, c.sectorsPerUnit); got != c.want {
			t.Errorf("allocationUnit(%d, %d) = %d, want %d",
				c.bytesPerSector, c.sectorsPerUnit, got, c.want)
		}
	}

	// The figure that started this: 7.56 TB of share reported as 3.8 TB.
	const units = 1_977_614_336 // allocation units on //TOWER/tower-vault
	total := int64(units) * allocationUnit(2048, 2)
	if tb := float64(total) / (1 << 40); tb < 7.3 || tb > 7.7 {
		t.Errorf("a 7.56 TB share came out as %.2f TB", tb)
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
	if _, _, ok := ro.Space(); ok {
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
