// Package mmc talks to an optical drive in its own language: SCSI
// Multi-Media Commands, sent straight to the device. It is what the drive
// answers to underneath /dev/sr0, and asking it directly is the only way to
// learn what a particular drive can and cannot do, to read a sector as the
// 2352 raw bytes it is on the disc rather than the 2048 the kernel hands
// back, and to tell a blank CD-R apart from an empty tray.
//
// Nothing here writes to a disc. Burning is left to a burner program that
// has met more drive firmware than this ever will; what this package
// contributes to a burn is the reading before and the verification after.
package mmc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Command opcodes, in the order the MMC specification lists them.
const (
	opTestUnitReady    = 0x00
	opInquiry          = 0x12
	opModeSense10      = 0x5a
	opPreventAllow     = 0x1e
	opStartStopUnit    = 0x1b
	opReadCapacity     = 0x25
	opRead10           = 0x28
	opSynchronizeCache = 0x35
	opReadTOC          = 0x43
	opGetConfiguration = 0x46
	opGetEventStatus   = 0x4a
	opReadDiscInfo     = 0x51
	opReadTrackInfo    = 0x52
	opMechanismStatus  = 0xbd
	opReadCD           = 0xbe
	opSetCDSpeed       = 0xbb
)

// Timeouts. A drive that has just been handed a disc spends whole seconds
// spinning it up and reading its lead-in before it will answer anything, and
// a read of a scratched sector is retried by the firmware for longer still.
const (
	shortTimeout = 20 * time.Second
	readTimeout  = 90 * time.Second
	trayTimeout  = 30 * time.Second
)

// ErrUnsupportedPlatform is returned by every entry point on a system with
// no SCSI generic interface. ripperX only ever runs on Linux in practice;
// this exists so the package still compiles and tests elsewhere.
var ErrUnsupportedPlatform = errors.New("talking to an optical drive directly is only implemented on Linux")

// Drive is one open optical drive. Commands are serialised: a drive has one
// mechanism and one buffer, and two callers interleaving a capability probe
// with a rip would get each other's data.
type Drive struct {
	path string

	mu sync.Mutex
	fd int
}

// Open opens the drive at path, which is a device node such as /dev/sr0.
// The device is opened non-blocking, so a drive with no disc, a blank disc
// or an unreadable disc opens exactly as readily as one with a CD in it.
func Open(path string) (*Drive, error) {
	fd, err := openDevice(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &Drive{path: path, fd: fd}, nil
}

func (d *Drive) Path() string { return d.path }

func (d *Drive) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fd < 0 {
		return nil
	}
	fd := d.fd
	d.fd = -1
	return closeDevice(fd)
}

// command sends one CDB and waits for it. A UNIT ATTENTION is retried once:
// the drive raises it to announce a disc change and discards the command
// that happened to arrive at that moment, so the first command after a swap
// would otherwise fail for no reason the caller can act on.
func (d *Drive) command(cdb []byte, data []byte, dir int32, timeout time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fd < 0 {
		return errors.New("the drive is closed")
	}
	err := sgSend(d.fd, cdb, data, dir, uint32(timeout.Milliseconds()))
	if IsUnitAttention(err) {
		err = sgSend(d.fd, cdb, data, dir, uint32(timeout.Milliseconds()))
	}
	return err
}

// in sends a command that reads data into buf.
func (d *Drive) in(cdb []byte, buf []byte, timeout time.Duration) error {
	return d.command(cdb, buf, sgDxferFromDev, timeout)
}

// out sends a command that carries no data.
func (d *Drive) out(cdb []byte, timeout time.Duration) error {
	return d.command(cdb, nil, sgDxferNone, timeout)
}

// Ready reports whether the drive has a disc it is prepared to read. The
// error is kept so the caller can say why not - an empty tray and a disc
// this drive cannot make sense of are different things to a user.
func (d *Drive) Ready() error {
	return d.out([]byte{opTestUnitReady, 0, 0, 0, 0, 0}, shortTimeout)
}

// Info is what the drive says it is.
type Info struct {
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Version string `json:"version"`
}

// String is the drive's name for a person. The fields are fixed-width and
// space-padded on the wire, and some drives pad in the middle as well -
// "DRW-24F1ST   a" - so runs of spaces are collapsed rather than shown.
func (i Info) String() string {
	return strings.Join(strings.Fields(i.Vendor+" "+i.Product), " ")
}

// Inquiry asks the drive for its identity. This is the one command every
// SCSI device answers, disc or no disc.
func (d *Drive) Inquiry() (Info, error) {
	buf := make([]byte, 96)
	cdb := []byte{opInquiry, 0, 0, 0, byte(len(buf)), 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return Info{}, err
	}
	if len(buf) < 36 {
		return Info{}, errors.New("INQUIRY: short answer")
	}
	return Info{
		Vendor:  strings.TrimSpace(string(buf[8:16])),
		Product: strings.TrimSpace(string(buf[16:32])),
		Version: strings.TrimSpace(string(buf[32:36])),
	}, nil
}

// LoadTray closes the tray, and Eject opens it. A drive with a slot loader
// answers the same commands; whether it has a tray at all is in Capabilities.
func (d *Drive) LoadTray() error {
	// Immediate off: wait for the disc to be spun up, so the next command
	// does not have to discover that it is not ready yet.
	return d.out([]byte{opStartStopUnit, 0, 0, 0, 0x03, 0}, trayTimeout)
}

func (d *Drive) Eject() error {
	if err := d.allowRemoval(); err != nil && !IsUnsupported(err) {
		return err
	}
	err := d.out([]byte{opStartStopUnit, 0, 0, 0, 0x02, 0}, trayTimeout)
	if !IsTrayStuck(err) {
		return err
	}
	// Some older drives refuse to release the tray while the disc is still
	// spinning, and say so as a mechanism failure. Stopping the unit first
	// and asking again is what they want; one drive here (a TSSTcorp
	// SH-S223C) needs it every time.
	if stopErr := d.out([]byte{opStartStopUnit, 0, 0, 0, 0x00, 0}, trayTimeout); stopErr != nil {
		return err // report the eject failure, not the spin-down
	}
	return d.out([]byte{opStartStopUnit, 0, 0, 0, 0x02, 0}, trayTimeout)
}

// allowRemoval undoes the lock the kernel takes while a filesystem on the
// disc is mounted, or that a previous program left behind. Without it a
// drive silently refuses to eject.
func (d *Drive) allowRemoval() error {
	return d.out([]byte{opPreventAllow, 0, 0, 0, 0, 0}, shortTimeout)
}

// SetSpeed asks the drive to read at no more than kbPerSec kilobytes a
// second; 0xffff means "as fast as you can". Slowing a drive down is the
// one thing that rescues a marginal disc: at 4x the servo tracks a scratch
// that it skates over at 48x. Not every drive honours it, and one that
// refuses outright is not an error worth failing a rip over.
func (d *Drive) SetSpeed(kbPerSec int) error {
	if kbPerSec <= 0 || kbPerSec > 0xffff {
		kbPerSec = 0xffff
	}
	cdb := []byte{opSetCDSpeed, 0,
		byte(kbPerSec >> 8), byte(kbPerSec),
		0xff, 0xff, // write speed: unchanged
		0, 0, 0, 0, 0, 0}
	err := d.out(cdb, shortTimeout)
	if IsUnsupported(err) {
		return nil
	}
	return err
}

// CDSpeedKB is one CD speed multiple, 1x, in kilobytes a second: 75 sectors
// of 2048 bytes each second. DVD and BD have their own, larger, 1x.
const CDSpeedKB = 176

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func putBE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}
