package mmc

import (
	"strings"
	"testing"
)

// The profile list is the drive's own statement of the disc kinds it
// handles, and the read/write matrices are derived from it. A mistake here
// shows up as ripperX offering to burn a disc the drive cannot write.
func TestProfilesDecideWhatTheDriveHandles(t *testing.T) {
	c := &Capabilities{Profiles: []Profile{
		ProfileCDROM, ProfileCDR, ProfileCDRW, ProfileDVDROM, ProfileDVDPlusRDL,
	}}
	c.applyProfiles()

	if !c.Read.CD || !c.Read.CDR || !c.Read.CDRW || !c.Read.DVD {
		t.Errorf("read support came out as %+v", c.Read)
	}
	if !c.Write.CDR || !c.Write.CDRW {
		t.Errorf("a drive listing CD-R and CD-RW profiles must be able to write them: %+v", c.Write)
	}
	if c.Write.BD || c.Write.HDDVD {
		t.Errorf("a drive that listed no Blu-ray profile must not claim to write one: %+v", c.Write)
	}
	if !c.Write.DualLayer || !c.Write.DVDPlusR {
		t.Errorf("a DVD+R DL profile implies both: %+v", c.Write)
	}
	// A read-only DVD-ROM profile must not become write support.
	only := &Capabilities{Profiles: []Profile{ProfileDVDROM}}
	only.applyProfiles()
	if only.Write.DVD {
		t.Error("DVD-ROM is a read-only profile")
	}
}

func TestProfileNaming(t *testing.T) {
	if got := ProfileCDRW.Name(); got != "CD-RW" {
		t.Errorf("CD-RW is called %q", got)
	}
	// A drive reporting something this table has never heard of is still
	// telling the truth, and the number is what a person would look up.
	if got := Profile(0xbeef).Name(); !strings.Contains(got, "beef") {
		t.Errorf("an unknown profile is called %q, which says nothing", got)
	}
	for _, p := range []Profile{ProfileCDROM, ProfileDVDROM, ProfileBDROM, ProfileNone} {
		if p.IsWritable() {
			t.Errorf("%s is not writable", p.Name())
		}
	}
	for _, p := range []Profile{ProfileCDR, ProfileCDRW, ProfileDVDPlusRW} {
		if !p.IsWritable() {
			t.Errorf("%s is writable", p.Name())
		}
	}
	if !ProfileCDR.IsCD() || ProfileDVDROM.IsCD() {
		t.Error("only a CD has CD sectors, and that is what raw reads depend on")
	}
}

// Every capability the probe tested has to land in exactly one of the two
// lists: a drive with an empty "cannot" must mean a drive that does
// everything, not a probe that gave up.
func TestSummariseAccountsForEverything(t *testing.T) {
	full := &Capabilities{
		Read:         MediaSupport{CD: true, CDRW: true, DVD: true, BD: true},
		Write:        MediaSupport{CDR: true, CDRW: true, DVDR: true, DVDRW: true, BD: true},
		CanReadRawCD: true, CanReadC2Errors: true, CanReadAccurateCDDA: true,
		CanReadMode2Form2: true, CanReadCDText: true, CanReadISRC: true,
		CanReadUPC: true, CanWriteSAO: true, CanWriteTAO: true,
		CanTestWrite: true, HasBurnProof: true, CanEject: true, IsMultiRead: true,
	}
	full.summarise()
	if len(full.Cannot) != 0 {
		t.Errorf("a drive that can do everything still reported %v", full.Cannot)
	}
	none := &Capabilities{}
	none.summarise()
	if len(none.Can) != 0 {
		t.Errorf("a drive that can do nothing still reported %v", none.Can)
	}
	if len(none.Cannot) != len(full.Can) {
		t.Errorf("the two lists have %d and %d entries; every capability must appear in exactly one",
			len(none.Cannot), len(full.Can))
	}

	// The raw-read line is the one a user acts on, so it has to say what the
	// consequence is rather than name the feature.
	joined := strings.Join(none.Cannot, "\n")
	if !strings.Contains(joined, "no audio, no raw image") {
		t.Errorf("the raw-read refusal does not say what it costs:\n%s", joined)
	}
}

func TestLoadingMechanismNames(t *testing.T) {
	if got := loadingMechanism(1); got != "tray" {
		t.Errorf("mechanism 1 is %q, want tray", got)
	}
	if got := loadingMechanism(7); got != "unknown" {
		t.Errorf("an unlisted mechanism is %q, want unknown", got)
	}
}

// The feature descriptors are variable length and self-describing; a body
// shorter than the fields read from it is what a malformed answer looks
// like, and it must not panic.
func TestApplyFeatureToleratesShortBodies(t *testing.T) {
	c := &Capabilities{}
	for _, code := range []uint16{
		featProfileList, featRemovableMedium, featCDRead, featDVDRead,
		featCDTrackAtOnce, featCDMastering, featDriveSerial,
	} {
		c.applyFeature(code, true, nil)
		c.applyFeature(code, true, []byte{0x01})
	}
}

func TestTrimmed(t *testing.T) {
	if got := trimmed([]byte("K15EAA82621   \x00\x00")); got != "K15EAA82621" {
		t.Errorf("trimmed gave %q", got)
	}
}

// Drives that have no serial number still answer the feature that reports
// one, with a body of padding and a byte or two of rubbish. Showing that as
// a serial is worse than showing none.
func TestSerialNumber(t *testing.T) {
	cases := []struct{ in, want string }{
		{"S10L68BDA00S3B", "S10L68BDA00S3B"},
		{"   \x00\x01  )U  ", ""},
		{"ab", ""},
		{"             ", ""},
		{"  K15EAA82621  ", "K15EAA82621"},
	}
	for _, tc := range cases {
		if got := serialNumber([]byte(tc.in)); got != tc.want {
			t.Errorf("serialNumber(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The model fields are fixed-width and padded, sometimes in the middle.
func TestInfoStringCollapsesPadding(t *testing.T) {
	got := Info{Vendor: "ASUS    ", Product: "DRW-24F1ST   a  "}.String()
	if got != "ASUS DRW-24F1ST a" {
		t.Errorf("drive name is %q", got)
	}
}
