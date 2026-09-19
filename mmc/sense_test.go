package mmc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Which layout a drive answers in is its own choice, and the three fields
// that matter sit in different places in each. Reading the wrong one turns
// "there is no disc" into a hardware error.
func TestParseSenseBothLayouts(t *testing.T) {
	fixed := make([]byte, 18)
	fixed[0] = 0x70
	fixed[2] = 0x02 // not ready
	fixed[12] = 0x3a
	fixed[13] = 0x00

	descriptor := []byte{0x72, 0x02, 0x3a, 0x00}

	for name, buf := range map[string][]byte{"fixed": fixed, "descriptor": descriptor} {
		err := parseSense(opTestUnitReady, buf, 0x02)
		var s *SenseError
		if !errors.As(err, &s) {
			t.Fatalf("%s: got %v, want a SenseError", name, err)
		}
		if s.Key != 0x02 || s.ASC != 0x3a || s.ASCQ != 0x00 {
			t.Errorf("%s: key %#x asc %#x/%#x", name, s.Key, s.ASC, s.ASCQ)
		}
		if !IsNoMedium(err) {
			t.Errorf("%s: an empty drive was not recognised as one", name)
		}
		if !strings.Contains(err.Error(), "no disc") {
			t.Errorf("%s: the message does not say what happened: %v", name, err)
		}
		if !strings.Contains(err.Error(), "TEST UNIT READY") {
			t.Errorf("%s: the message does not name the command: %v", name, err)
		}
	}
}

func TestSenseClassification(t *testing.T) {
	sense := func(key, asc, ascq byte) error {
		return &SenseError{Op: opRead10, Status: 0x02, Key: key, ASC: asc, ASCQ: ascq}
	}
	cases := []struct {
		name string
		err  error
		is   func(error) bool
		want bool
	}{
		{"empty tray", sense(0x02, 0x3a, 0x00), IsNoMedium, true},
		{"spinning up", sense(0x02, 0x04, 0x01), IsNoMedium, true},
		{"disc swapped", sense(0x06, 0x28, 0x00), IsUnitAttention, true},
		{"command unknown", sense(0x05, 0x20, 0x00), IsUnsupported, true},
		{"field invalid", sense(0x05, 0x24, 0x00), IsUnsupported, true},
		{"unreadable sector", sense(0x03, 0x11, 0x00), IsReadError, true},
		{"wrong track type", sense(0x05, 0x64, 0x00), IsReadError, true},
		{"a read error is not an empty tray", sense(0x03, 0x11, 0x00), IsNoMedium, false},
		{"an empty tray is not unsupported", sense(0x02, 0x3a, 0x00), IsUnsupported, false},
		{"a plain error is nothing in particular", errors.New("boom"), IsReadError, false},
	}
	for _, tc := range cases {
		if got := tc.is(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The helpers are used through wrapped errors all over the rip paths, so
// they have to see through a %w.
func TestSenseSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("reading sector %d: %w", 1234,
		&SenseError{Op: opReadCD, Key: 0x03, ASC: 0x11})
	if !IsReadError(err) {
		t.Error("a wrapped read error was not recognised")
	}
}

func TestUnparsableSenseStillSaysSomething(t *testing.T) {
	err := parseSense(opInquiry, nil, 0x02)
	if err == nil || !strings.Contains(err.Error(), "no sense data") {
		t.Errorf("got %v, want a complaint about the missing sense data", err)
	}
	if sense(err) != nil {
		t.Error("an unparsable answer must not masquerade as a decoded one")
	}
}

// A drive that will not move its tray is told apart from one that has
// failed, because the first is worth retrying after a spin-down and the
// second is not.
func TestTrayStuck(t *testing.T) {
	stuck := &SenseError{Op: opStartStopUnit, Key: 0x04, ASC: 0x53, ASCQ: 0x00}
	if !IsTrayStuck(stuck) {
		t.Error("a mechanism failure on the tray was not recognised")
	}
	if !strings.Contains(stuck.Error(), "could not move the tray") {
		t.Errorf("the message does not say what happened: %v", stuck)
	}
	if IsTrayStuck(&SenseError{Key: 0x04, ASC: 0x44}) {
		t.Error("an internal failure is not a stuck tray")
	}
}

// A drive declines a C2 read within a few sectors of the lead-out and says
// "aborted command". Treating that as fatal ends a surface scan 22 sectors
// from the end of every disc, so it has to count as a read error - which
// means the sector is narrowed down and retried rather than the job dying.
func TestAbortedCommandIsARetryableReadError(t *testing.T) {
	aborted := &SenseError{Op: opReadCD, Status: 0x02, Key: 0x0b, ASC: 0x00, ASCQ: 0x00}
	if !IsReadError(aborted) {
		t.Error("an aborted read must be narrowed down, not fatal")
	}
	// It is still not any of the other things, so nothing else changes
	// behaviour because of it.
	if IsNoMedium(aborted) || IsUnsupported(aborted) || IsUnitAttention(aborted) {
		t.Error("an aborted read was classified as something else as well")
	}
	// An illegal request that is not the track-type one stays fatal: that is
	// a command this drive will never accept, and retrying is pointless.
	if IsReadError(&SenseError{Key: 0x05, ASC: 0x20}) {
		t.Error("an unimplemented command must not be retried as a read error")
	}
}
