package main

import (
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/mmc"
)

func TestDiscVolumeName(t *testing.T) {
	cases := map[string]string{
		"Terminator-5v1-2of2-20260919-182903.zip": "TERMINATOR_5V1_2OF2",
		"DMC_STRA.tar.gz":                         "DMC_STRA",
		"Наклейка.zip":                            "DISC",
		"....zip":                                 "DISC",
		strings.Repeat("x", 80) + ".zip":          strings.Repeat("X", 32),
	}
	for in, want := range cases {
		if got := discVolumeName(in); got != want {
			t.Errorf("discVolumeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The size of what is in an archive is not known until it has been opened,
// so this is the check that stands between it and a spoiled disc.
func TestFilesFitTheDisc(t *testing.T) {
	cd := &mmc.Disc{BlankSectors: 360000} // an 80-minute CD-R
	if err := checkFilesFit(700<<20, cd); err == nil {
		t.Error("700 MB was accepted onto a 737 MB disc, with no room for the filesystem")
	}
	if err := checkFilesFit(600<<20, cd); err != nil {
		t.Errorf("600 MB should fit an 80-minute CD: %v", err)
	}
	// A disc with a session already on it is measured by what is left.
	part := &mmc.Disc{Sectors: 2295104, WritableBytes: 300 << 20}
	if err := checkFilesFit(400<<20, part); err == nil {
		t.Error("400 MB was accepted into 300 MB of free space")
	}
	if err := checkFilesFit(200<<20, part); err != nil {
		t.Errorf("200 MB should fit in 300 MB: %v", err)
	}
}
