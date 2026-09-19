package mmc

import (
	"errors"
	"fmt"
	"math/bits"
)

// How many sectors to ask for in one command. The ioctl has a ceiling on
// how much it will move in one go - a few hundred kilobytes, depending on
// the host adapter - and asking for more than the drive will give fails the
// whole command rather than returning short. These sizes are comfortably
// under every ceiling seen in practice while still amortising the
// per-command cost over a useful amount of data.
const (
	ChunkData = 64 // 128 KiB
	ChunkRaw  = 32 // 73.5 KiB
)

// ReadData reads count sectors of user data from lba into buf, which must
// hold count*SectorData bytes. This is the ordinary read: the drive
// corrects the sector and hands back the 2048 bytes a filesystem sees.
func (d *Drive) ReadData(lba int64, count int, buf []byte) error {
	if count <= 0 {
		return nil
	}
	if len(buf) < count*SectorData {
		return fmt.Errorf("ReadData: buffer holds %d bytes, need %d", len(buf), count*SectorData)
	}
	cdb := make([]byte, 10)
	cdb[0] = opRead10
	putBE32(cdb[2:6], uint32(lba))
	cdb[7], cdb[8] = byte(count>>8), byte(count)
	return d.in(cdb, buf[:count*SectorData], readTimeout)
}

// SectorType selects which kind of sector READ CD is asked for. Any is
// right for a data disc; CDDA is what an audio track must be read as,
// because an audio sector has no header for the drive to check the type
// against.
type SectorType byte

const (
	SectorAny   SectorType = 0
	SectorCDDA  SectorType = 1
	SectorMode1 SectorType = 2
)

// ReadRaw reads count sectors from lba as the full 2352 bytes they are on
// the disc: sync pattern, header, user data and error correction. buf must
// hold count*SectorRaw bytes.
//
// This is what a .img is made of, and what an audio track is. Not every
// drive will do it - Capabilities.CanReadRawCD says whether - and no drive
// will do it for a DVD, where the sector is not a CD sector at all.
func (d *Drive) ReadRaw(lba int64, count int, t SectorType, buf []byte) error {
	if count <= 0 {
		return nil
	}
	if len(buf) < count*SectorRaw {
		return fmt.Errorf("ReadRaw: buffer holds %d bytes, need %d", len(buf), count*SectorRaw)
	}
	cdb := make([]byte, 12)
	cdb[0] = opReadCD
	cdb[1] = byte(t) << 2
	putBE32(cdb[2:6], uint32(lba))
	cdb[6], cdb[7], cdb[8] = byte(count>>16), byte(count>>8), byte(count)
	// Sync, both header fields, user data and error correction: everything,
	// which is what adds up to 2352. No subchannel.
	cdb[9] = 0xf8
	return d.in(cdb, buf[:count*SectorRaw], readTimeout)
}

// ErrSectorUnreadable is a sector the drive gave up on after its own
// retries. A rip records where it happened and carries on, because one bad
// sector should not cost the rest of the disc.
var ErrSectorUnreadable = errors.New("the drive could not read this sector")

// ReadDataRetry reads count sectors and, when the drive refuses the batch,
// narrows down to find which sectors are actually bad. The readable ones
// are returned in place and the unreadable ones are zero-filled; bad lists
// their addresses. The error is non-nil only when the failure was not a
// read error - a drive that has gone away, say.
//
// Splitting rather than failing is the whole point: a disc with a scratch
// across one file still yields the other four hundred, and the caller is
// told exactly which bytes are made up.
func (d *Drive) ReadDataRetry(lba int64, count int, buf []byte) (bad []int64, err error) {
	if err := d.ReadData(lba, count, buf); err == nil {
		return nil, nil
	} else if !IsReadError(err) {
		return nil, err
	}
	if count == 1 {
		clear(buf[:SectorData])
		return []int64{lba}, nil
	}
	half := count / 2
	first, rest := buf[:half*SectorData], buf[half*SectorData:]
	b1, err := d.ReadDataRetry(lba, half, first)
	if err != nil {
		return nil, err
	}
	b2, err := d.ReadDataRetry(lba+int64(half), count-half, rest)
	if err != nil {
		return nil, err
	}
	return append(b1, b2...), nil
}

// ReadRawRetry is ReadDataRetry for raw sectors.
func (d *Drive) ReadRawRetry(lba int64, count int, t SectorType, buf []byte) (bad []int64, err error) {
	if err := d.ReadRaw(lba, count, t, buf); err == nil {
		return nil, nil
	} else if !IsReadError(err) {
		return nil, err
	}
	if count == 1 {
		clear(buf[:SectorRaw])
		return []int64{lba}, nil
	}
	half := count / 2
	first, rest := buf[:half*SectorRaw], buf[half*SectorRaw:]
	b1, err := d.ReadRawRetry(lba, half, t, first)
	if err != nil {
		return nil, err
	}
	b2, err := d.ReadRawRetry(lba+int64(half), count-half, t, rest)
	if err != nil {
		return nil, err
	}
	return append(b1, b2...), nil
}

// ProbeRaw reports whether this drive will actually hand over raw sectors
// from the disc that is in it. Capabilities says what the drive claims;
// this tries it, because the claim and the behaviour differ often enough
// to be worth one command before a rip that would otherwise fail minutes in.
func (d *Drive) ProbeRaw(lba int64, t SectorType) bool {
	buf := make([]byte, SectorRaw)
	return d.ReadRaw(lba, 1, t, buf) == nil
}

// C2 error pointers are the drive's own account of how well it is reading a
// disc. For every one of the 2352 bytes in a sector the drive sets a bit
// saying whether that byte came back uncorrected by the CIRC error
// correction - so 294 bytes of flags ride behind each sector.
//
// This is the only portable measure of a CD's condition there is. It is not
// a guess from read speeds or a count of outright failures: a disc can read
// perfectly and still be one summer away from being unreadable, and the C2
// rate is what shows that. Not every drive reports them; Capabilities says
// which, and both drives here do.
const (
	// C2Bytes is the flag block that follows each sector: one bit per byte.
	C2Bytes = 294
	// SectorRawC2 is a raw sector with its flags behind it.
	SectorRawC2 = SectorRaw + C2Bytes
	// ChunkC2 is smaller than ChunkRaw because each sector costs more.
	ChunkC2 = 24
)

// ReadRawC2 reads count sectors as raw data followed by their C2 flags. buf
// must hold count*SectorRawC2 bytes.
func (d *Drive) ReadRawC2(lba int64, count int, t SectorType, buf []byte) error {
	if count <= 0 {
		return nil
	}
	if len(buf) < count*SectorRawC2 {
		return fmt.Errorf("ReadRawC2: buffer holds %d bytes, need %d", len(buf), count*SectorRawC2)
	}
	cdb := make([]byte, 12)
	cdb[0] = opReadCD
	cdb[1] = byte(t) << 2
	putBE32(cdb[2:6], uint32(lba))
	cdb[6], cdb[7], cdb[8] = byte(count>>16), byte(count>>8), byte(count)
	// As ReadRaw, plus 01b in the error flags field, which appends the 294
	// bytes of C2 pointers.
	cdb[9] = 0xf8 | 0x02
	return d.in(cdb, buf[:count*SectorRawC2], readTimeout)
}

// CountC2 returns how many bytes of one sector the drive could not correct,
// given that sector's 294 flag bytes. Zero is a sector that came off the
// disc cleanly.
func CountC2(flags []byte) int {
	n := 0
	for _, b := range flags {
		n += bits.OnesCount8(b)
	}
	return n
}

// SplitC2 takes one sector out of a ReadRawC2 buffer: its 2352 bytes of
// data and its 294 bytes of flags.
func SplitC2(buf []byte, i int) (data, flags []byte) {
	off := i * SectorRawC2
	return buf[off : off+SectorRaw], buf[off+SectorRaw : off+SectorRawC2]
}

// ProbeC2 reports whether this drive will actually return C2 pointers for
// the disc in it. As with raw reads, the capability page and the behaviour
// differ often enough that one command is worth it before a scan that would
// otherwise measure nothing.
func (d *Drive) ProbeC2(lba int64, t SectorType) bool {
	buf := make([]byte, SectorRawC2)
	return d.ReadRawC2(lba, 1, t, buf) == nil
}
