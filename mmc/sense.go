package mmc

import (
	"errors"
	"fmt"
)

// SenseError is a drive answering CHECK CONDITION: the command reached the
// drive and the drive declined it. The sense key and the additional sense
// code say why, and for an optical drive most of those reasons are ordinary
// facts rather than faults - there is no disc, the disc was swapped, the
// drive is still spinning up. Callers test for them with the Is* helpers
// instead of matching on strings.
type SenseError struct {
	Op     byte // the command that was refused
	Status byte // SCSI status, normally 0x02 CHECK CONDITION
	Key    byte
	ASC    byte
	ASCQ   byte
}

func (e *SenseError) Error() string {
	if d := senseText[[2]byte{e.ASC, e.ASCQ}]; d != "" {
		return fmt.Sprintf("%s: %s (sense %s, asc %02x/%02x)", opName(e.Op), d, senseKeyName(e.Key), e.ASC, e.ASCQ)
	}
	return fmt.Sprintf("%s: %s (asc %02x/%02x)", opName(e.Op), senseKeyName(e.Key), e.ASC, e.ASCQ)
}

// parseSense turns a sense buffer into a SenseError. Both the fixed and the
// descriptor layout are accepted: which one a drive returns is its own
// choice, and the three fields we want sit in different places in each.
func parseSense(op byte, sense []byte, status uint8) error {
	e := &SenseError{Op: op, Status: status}
	switch {
	case len(sense) >= 14 && sense[0]&0x7e == 0x70: // fixed, 0x70 or 0x71
		e.Key, e.ASC, e.ASCQ = sense[2]&0x0f, sense[12], sense[13]
	case len(sense) >= 4 && sense[0]&0x7e == 0x72: // descriptor, 0x72 or 0x73
		e.Key, e.ASC, e.ASCQ = sense[1]&0x0f, sense[2], sense[3]
	case len(sense) == 0:
		return fmt.Errorf("%s: status %#02x with no sense data", opName(op), status)
	default:
		return fmt.Errorf("%s: status %#02x, unparsable sense %x", opName(op), status, sense)
	}
	return e
}

func sense(err error) *SenseError {
	var s *SenseError
	if errors.As(err, &s) {
		return s
	}
	return nil
}

// IsNoMedium reports whether the drive is empty, or has a disc it has not
// managed to read the shape of yet. Both mean "nothing to offer here", and
// both are normal.
func IsNoMedium(err error) bool {
	s := sense(err)
	if s == nil {
		return false
	}
	switch {
	case s.Key == 0x02 && s.ASC == 0x3a: // medium not present
		return true
	case s.Key == 0x02 && s.ASC == 0x04 && s.ASCQ == 0x01: // becoming ready
		return true
	case s.Key == 0x02 && s.ASC == 0x30: // incompatible or unformatted medium
		return true
	}
	return false
}

// IsUnitAttention reports the drive announcing that something changed under
// it - almost always a disc swapped since the last command. The command that
// got this answer was not executed and is worth retrying once.
func IsUnitAttention(err error) bool {
	s := sense(err)
	return s != nil && s.Key == 0x06
}

// IsUnsupported reports a command or a field this drive does not implement.
// Older drives answer this to half of GET CONFIGURATION's relatives, so a
// capability probe treats it as "no" rather than as an error.
func IsUnsupported(err error) bool {
	s := sense(err)
	if s == nil {
		return false
	}
	return s.Key == 0x05 && (s.ASC == 0x20 || s.ASC == 0x24 || s.ASC == 0x26)
}

// IsTrayStuck reports the drive refusing to move its tray. Some older
// drives answer this to an eject that arrives while the disc is still
// spinning, and will do it once the unit has been stopped - so it is worth
// distinguishing from a drive that has genuinely failed.
func IsTrayStuck(err error) bool {
	s := sense(err)
	return s != nil && s.ASC == 0x53
}

// IsReadError reports a sector the drive would not give up. Ripping treats
// this per sector rather than abandoning the disc: a scratch in one file
// should not cost the other four hundred.
//
// "Aborted command" is in here because that is what a drive answers when it
// declines a read it could in principle do - notably a raw read with C2
// pointers within a few sectors of the lead-out, where the firmware has no
// read-ahead left to work with. Treating it as fatal would end a scan 22
// sectors from the end of every disc.
func IsReadError(err error) bool {
	s := sense(err)
	if s == nil {
		return false
	}
	switch s.Key {
	case 0x03, 0x04, 0x0b: // medium error, hardware error, aborted command
		return true
	case 0x05:
		return s.ASC == 0x64 // illegal mode for this track
	}
	return false
}

func senseKeyName(k byte) string {
	names := [...]string{
		"no sense", "recovered error", "not ready", "medium error",
		"hardware error", "illegal request", "unit attention", "data protect",
		"blank check", "vendor specific", "copy aborted", "aborted command",
		"equal", "volume overflow", "miscompare", "completed",
	}
	if int(k) < len(names) {
		return names[k]
	}
	return fmt.Sprintf("sense key %#x", k)
}

// senseText covers the codes a CD drive actually produces in normal use. An
// unlisted code still reports its number, which is enough to look up.
var senseText = map[[2]byte]string{
	{0x04, 0x01}: "the drive is still becoming ready",
	{0x04, 0x02}: "the drive needs an initialising command",
	{0x04, 0x04}: "a format is in progress",
	{0x05, 0x00}: "the drive does not support this logical unit",
	{0x00, 0x00}: "the drive declined the command without saying why",
	{0x0c, 0x00}: "the write failed",
	{0x11, 0x00}: "unrecovered read error",
	{0x11, 0x05}: "L-EC uncorrectable error",
	{0x11, 0x06}: "CIRC uncorrectable error",
	{0x1a, 0x00}: "the parameter list is the wrong length",
	{0x20, 0x00}: "the drive does not implement this command",
	{0x21, 0x00}: "the address is past the end of the disc",
	{0x24, 0x00}: "a field in the command is invalid for this drive",
	{0x26, 0x00}: "a field in the parameters is invalid for this drive",
	{0x28, 0x00}: "the disc was changed",
	{0x29, 0x00}: "the drive was reset",
	{0x2c, 0x00}: "the command is out of sequence here",
	{0x30, 0x00}: "the disc is of a kind this drive cannot use",
	{0x30, 0x01}: "the disc cannot be read - wrong kind for this drive",
	{0x30, 0x02}: "the disc cannot be written - wrong kind for this drive",
	{0x30, 0x05}: "the disc cannot be erased",
	{0x30, 0x07}: "the disc has to be blanked first",
	{0x30, 0x10}: "the disc is not fully formatted",
	{0x08, 0x00}: "the drive stopped responding",
	{0x09, 0x00}: "the drive could not follow the track",
	{0x15, 0x01}: "the drive's mechanism could not position itself",
	{0x31, 0x00}: "the disc's format is corrupted",
	{0x3a, 0x00}: "there is no disc in the drive",
	{0x3a, 0x01}: "the tray is closed and empty",
	{0x3a, 0x02}: "the tray is open",
	{0x44, 0x00}: "the drive reported an internal failure",
	{0x51, 0x00}: "the erase failed",
	{0x53, 0x00}: "the drive could not move the tray",
	{0x53, 0x02}: "the tray is locked by something else",
	{0x57, 0x00}: "the drive cannot read the disc's table of contents",
	{0x63, 0x00}: "the disc has no track at this address",
	{0x64, 0x00}: "this track is of the wrong kind for this command",
	{0x64, 0x01}: "the sector is not in the expected format",
	{0x72, 0x03}: "the session is not in a state that allows this",
	{0x73, 0x03}: "the power calibration area is full - the disc is worn out",
}

func opName(op byte) string {
	if n := opNames[op]; n != "" {
		return n
	}
	return fmt.Sprintf("command %#02x", op)
}

var opNames = map[byte]string{
	opTestUnitReady:    "TEST UNIT READY",
	opInquiry:          "INQUIRY",
	opPreventAllow:     "PREVENT ALLOW MEDIUM REMOVAL",
	opStartStopUnit:    "START STOP UNIT",
	opReadCapacity:     "READ CAPACITY",
	opRead10:           "READ(10)",
	opSynchronizeCache: "SYNCHRONIZE CACHE",
	opReadTOC:          "READ TOC/PMA/ATIP",
	opModeSense10:      "MODE SENSE(10)",
	opGetConfiguration: "GET CONFIGURATION",
	opGetEventStatus:   "GET EVENT STATUS NOTIFICATION",
	opReadDiscInfo:     "READ DISC INFORMATION",
	opReadTrackInfo:    "READ TRACK INFORMATION",
	opSetCDSpeed:       "SET CD SPEED",
	opReadCD:           "READ CD",
	opMechanismStatus:  "MECHANISM STATUS",
}
