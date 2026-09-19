//go:build !linux

package mmc

// Everything here fails the same way, and Open fails first, so no caller
// ever reaches the rest. Optical drives are reachable this way only through
// Linux's SCSI generic interface; the package exists on other systems so
// that the rest of ripperX still builds and tests there.
const (
	sgDxferNone    = 0
	sgDxferToDev   = 0
	sgDxferFromDev = 0
)

func sgSend(fd int, cdb []byte, data []byte, dir int32, timeoutMS uint32) error {
	return ErrUnsupportedPlatform
}

func openDevice(path string) (int, error) { return -1, ErrUnsupportedPlatform }

func closeDevice(fd int) error { return nil }

func parseSenseUnused() {}
