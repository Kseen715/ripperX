//go:build linux

package mmc

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The Linux SCSI generic interface: one ioctl that carries a command block
// down to the drive and brings its data and its sense back. It is what
// cdrecord, xorriso and the kernel's own CD driver all speak underneath, and
// going to it directly is what lets ripperX ask a drive what it is - rather
// than guess from the handful of things /dev/sr0 exposes as an ordinary file.
const (
	sgIO        = 0x2285 // SG_IO
	sgVersion   = 30000
	sgInterface = 'S'

	sgDxferNone    = -1
	sgDxferToDev   = -2
	sgDxferFromDev = -3

	senseLen = 32
)

// sgIOHdr mirrors the kernel's sg_io_hdr_t. The field order and the implicit
// padding Go inserts before usrPtr match the C struct on both 32- and 64-bit
// builds, which is what makes the same declaration work across every
// architecture the release builds cover.
type sgIOHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         *byte
	cmdp           *byte
	sbp            *byte
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         *byte
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

// ptr returns a pointer to the first byte of b, or nil for an empty buffer.
// A nil dxferp with a zero length is how a command that moves no data is
// expressed; handing the kernel a pointer to a zero-length slice is not.
func ptr(b []byte) *byte {
	if len(b) == 0 {
		return nil
	}
	return &b[0]
}

// sgSend issues one command. data is the buffer read into or written from,
// and dir says which. A drive that answers CHECK CONDITION comes back as a
// SenseError carrying the key and the ASC/ASCQ, because for a CD drive that
// is information rather than a failure: "no medium present" and "medium may
// have changed" are ordinary answers to an ordinary question.
func sgSend(fd int, cdb []byte, data []byte, dir int32, timeoutMS uint32) error {
	sense := make([]byte, senseLen)
	hdr := sgIOHdr{
		interfaceID:    sgInterface,
		dxferDirection: dir,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        senseLen,
		dxferLen:       uint32(len(data)),
		dxferp:         ptr(data),
		cmdp:           ptr(cdb),
		sbp:            ptr(sense),
		timeout:        timeoutMS,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), sgIO, uintptr(unsafe.Pointer(&hdr)))
	// The buffers are reachable only through the header the kernel was handed,
	// so they must be kept alive across the call explicitly.
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(data)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return fmt.Errorf("SG_IO: %w", errno)
	}
	if hdr.hostStatus != 0 || hdr.driverStatus&^0x08 != 0 {
		return fmt.Errorf("SG_IO: transport failed (host %#x, driver %#x)", hdr.hostStatus, hdr.driverStatus)
	}
	if hdr.status != 0 {
		return parseSense(cdb[0], sense[:min(int(hdr.sbLenWr), senseLen)], hdr.status)
	}
	return nil
}

// openDevice opens the drive for the SG_IO ioctl. O_NONBLOCK matters: without
// it the open blocks until a disc is loaded and readable, so a drive with a
// blank or an unreadable disc in it could not even be asked what it is.
func openDevice(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}

func closeDevice(fd int) error { return unix.Close(fd) }
