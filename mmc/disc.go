package mmc

import (
	"errors"
	"fmt"
	"time"
)

// Sector sizes. A CD sector is 2352 bytes on the disc; what a drive hands
// back depends on which of those bytes it decodes for you.
const (
	// SectorData is a Mode 1 or Mode 2 Form 1 data sector with its sync,
	// header and error correction stripped: what a filesystem sees, and what
	// an .iso is made of.
	SectorData = 2048
	// SectorRaw is the whole sector as it is on the disc, including the sync
	// pattern, the header and the error correction. This is what an .img
	// preserves and what an audio track is.
	SectorRaw = 2352
	// FramesPerSecond and the frame layout of CD addressing: 75 sectors a
	// second, and the first 2 seconds of every disc are the lead-in, which
	// is why an audio track's address is 150 sectors ahead of its offset in
	// the ripped file.
	FramesPerSecond = 75
	LeadInSectors   = 150
)

// DiscStatus is how far along a disc is: whether it can still be written
// to, and whether it has been closed.
type DiscStatus int

const (
	DiscEmpty      DiscStatus = 0 // blank, nothing written
	DiscAppendable DiscStatus = 1 // has data and an open session
	DiscComplete   DiscStatus = 2 // finalised
	DiscOther      DiscStatus = 3 // random writable, e.g. DVD-RAM
)

func (s DiscStatus) String() string {
	switch s {
	case DiscEmpty:
		return "blank"
	case DiscAppendable:
		return "appendable"
	case DiscComplete:
		return "closed"
	default:
		return "random writable"
	}
}

// Disc is everything worth knowing about the disc that is in the drive now.
// A zero Present means the tray is empty or the disc is unreadable, and
// Error then says which.
type Disc struct {
	Present bool   `json:"present"`
	Error   string `json:"error,omitempty"`

	Profile     Profile    `json:"profile"`
	ProfileName string     `json:"profileName"`
	Status      DiscStatus `json:"status"`
	StatusName  string     `json:"statusName"`
	Erasable    bool       `json:"erasable"`
	Sessions    int        `json:"sessions"`

	// Sectors is the number of addressable sectors, and BlockSize what the
	// drive says one of them is - 2048 for a data disc, 2352 for an audio
	// disc that the drive reports in raw units.
	Sectors   int64 `json:"sectors"`
	BlockSize int   `json:"blockSize"`
	// DataBytes is Sectors at 2048, the size of an .iso of this disc, and
	// RawBytes is Sectors at 2352, the size of an .img.
	DataBytes int64 `json:"dataBytes"`
	RawBytes  int64 `json:"rawBytes"`

	// RawReadable is whether this drive will actually hand over 2352-byte
	// sectors from this disc, established by trying it rather than by
	// believing the capability page - drives lie about this in both
	// directions. It is what decides whether a raw .img and an audio rip
	// are offered at all.
	RawReadable bool `json:"rawReadable"`

	Tracks []Track `json:"tracks"`
	// AudioTracks and DataTracks are counted out so a page can decide what
	// to offer without walking the list.
	AudioTracks int `json:"audioTracks"`
	DataTracks  int `json:"dataTracks"`

	// A disc that is written but not closed can take another session. These
	// say whether, where it would go, and how much room is left - which is
	// the difference between "this disc is full" and "there is a third of a
	// gigabyte going spare".
	// FreeSpaceError is why the room left could not be established, when it
	// could not. A disc with an open session whose free space is unknown is
	// not the same thing as a full one, and saying "full" about the first
	// was wrong.
	FreeSpaceError string `json:"freeSpaceError,omitempty"`

	// lastTrack is the last track of the last session, as the drive reports
	// it. It is not shown - it exists because it is the only reliable way to
	// name the track that has not been written yet.
	lastTrack int

	// LastSessionStart is the first sector of the most recent session. On a
	// multi-session disc that is where the filesystem to read lives: the
	// descriptors at the start of the disc describe the first session only,
	// and a later session's directory refers back to the earlier data.
	LastSessionStart int64 `json:"lastSessionStart"`

	Appendable      bool  `json:"appendable"`
	NextWritable    int64 `json:"nextWritable,omitempty"`
	WritableSectors int64 `json:"writableSectors,omitempty"`
	WritableBytes   int64 `json:"writableBytes,omitempty"`

	// MediaID is the disc's manufacturer, from the ATIP on a recordable CD.
	// Blank discs have nothing else to identify them by.
	MediaID string `json:"mediaId,omitempty"`
	// Capacity of a blank recordable disc, from the ATIP lead-out address.
	BlankSectors int64 `json:"blankSectors,omitempty"`
}

// Track is one track of the disc's table of contents.
type Track struct {
	Number  int   `json:"number"`
	Audio   bool  `json:"audio"`
	Start   int64 `json:"start"`   // first sector
	Sectors int64 `json:"sectors"` // length in sectors
	// Bytes is the track's size in the form it would be ripped as: 2352 for
	// audio, 2048 for data.
	Bytes int64 `json:"bytes"`
	// Duration is meaningful for audio tracks, where 75 sectors is a second.
	DurationSeconds float64 `json:"durationSeconds"`
	PreEmphasis     bool    `json:"preEmphasis,omitempty"`
	FourChannel     bool    `json:"fourChannel,omitempty"`
	CopyPermitted   bool    `json:"copyPermitted,omitempty"`
}

// ReadDisc gathers the disc's identity, its table of contents and its size.
// An empty drive is not an error: Present is false and Error says why, so
// the UI can show an idle drive alongside a busy one.
func (d *Drive) ReadDisc() (*Disc, error) {
	disc := &Disc{}

	// The current profile says what kind of disc this is; it is also the one
	// question that works on a blank.
	if caps, err := d.currentProfile(); err == nil {
		disc.Profile = caps
	}
	disc.ProfileName = disc.Profile.Name()

	if err := d.Ready(); err != nil {
		if IsNoMedium(err) {
			disc.Error = err.Error()
			// Some drives answer "still becoming ready" indefinitely once
			// their tray has been cycled with nothing in it - one here, a
			// TSSTcorp SH-S223C, never stops. Asking outright whether there
			// is a disc settles it, and turns a message that reads like a
			// drive halfway through something into the plain fact.
			if _, present, e := d.MediaChanged(); e == nil && !present {
				disc.Error = "there is no disc in the drive"
			}
			return disc, nil
		}
		// Anything else is a drive problem rather than a disc problem.
		return nil, err
	}
	disc.Present = true

	if err := d.readDiscInfo(disc); err != nil && !IsUnsupported(err) {
		disc.Error = err.Error()
	}
	disc.StatusName = disc.Status.String()

	// A blank disc has no capacity and no table of contents; its size comes
	// from the ATIP instead.
	if disc.Status == DiscEmpty {
		if err := d.readATIP(disc); err != nil && !IsUnsupported(err) && !IsNoMedium(err) {
			disc.Error = err.Error()
		}
		return disc, nil
	}

	if err := d.readCapacity(disc); err != nil {
		disc.Error = err.Error()
	}
	if err := d.readTOC(disc); err != nil && !IsUnsupported(err) {
		if disc.Error == "" {
			disc.Error = err.Error()
		}
	}
	if err := d.readSessionInfo(disc); err != nil && !IsUnsupported(err) {
		// Not fatal: a single-session disc starts at zero, which is what
		// the field already says.
		if disc.Error == "" {
			disc.Error = err.Error()
		}
	}
	// Prefer the table of contents for the disc's length: READ CAPACITY on a
	// mixed or audio disc often reports only up to the first data track's
	// end, while the lead-out address is the whole disc.
	if n := disc.tocEnd(); n > disc.Sectors {
		disc.Sectors = n
	}
	// How much room is left, if any. A disc that is closed has none and
	// says so by refusing the question, which is not an error.
	if disc.Status == DiscAppendable {
		if err := d.readTrackInfo(disc); err != nil {
			// Not fatal: the disc is still perfectly readable. But it is
			// recorded rather than swallowed, because "the drive would not
			// say" and "there is no room" are different answers and only
			// one of them means stop.
			disc.FreeSpaceError = err.Error()
		} else if disc.WritableSectors == 0 && !disc.Appendable {
			disc.FreeSpaceError = "the drive did not report a next writable address"
		}
	}

	// Ask the disc rather than the drive's self-description: only a CD has
	// 2352-byte sectors at all, and among CDs the capability page and the
	// behaviour disagree often enough that one test read is worth it.
	if disc.Profile.IsCD() {
		start := int64(0)
		t := SectorAny
		if len(disc.Tracks) > 0 {
			start = disc.Tracks[0].Start
			if disc.Tracks[0].Audio {
				t = SectorCDDA
			}
		}
		disc.RawReadable = d.ProbeRaw(start, t)
	}
	disc.DataBytes = disc.Sectors * SectorData
	disc.RawBytes = disc.Sectors * SectorRaw
	return disc, nil
}

// tocEnd is the lead-out address: the first sector past the last track.
func (d *Disc) tocEnd() int64 {
	var end int64
	for _, t := range d.Tracks {
		if e := t.Start + t.Sectors; e > end {
			end = e
		}
	}
	return end
}

func (d *Drive) currentProfile() (Profile, error) {
	head := make([]byte, 8)
	if err := d.in(configCDB(len(head)), head, shortTimeout); err != nil {
		return ProfileNone, err
	}
	return Profile(be16(head[6:8])), nil
}

func (d *Drive) readDiscInfo(disc *Disc) error {
	buf := make([]byte, 34)
	cdb := []byte{opReadDiscInfo, 0, 0, 0, 0, 0, 0, byte(len(buf) >> 8), byte(len(buf)), 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return err
	}
	if len(buf) < 12 {
		return errors.New("READ DISC INFORMATION: short answer")
	}
	disc.Status = DiscStatus(buf[2] & 0x03)
	disc.Erasable = buf[2]&0x10 != 0
	disc.Sessions = int(buf[9])<<8 | int(buf[4])
	// The last track of the last session. On a disc that can still be
	// written this is the track that does not exist yet, and it is the only
	// dependable way to name it: the table of contents does not necessarily
	// list it, and one drive here refuses the 0xff convention outright.
	disc.lastTrack = int(buf[11])<<8 | int(buf[6])
	return nil
}

func (d *Drive) readCapacity(disc *Disc) error {
	buf := make([]byte, 8)
	cdb := []byte{opReadCapacity, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return err
	}
	last := int64(be32(buf[0:4]))
	disc.BlockSize = int(be32(buf[4:8]))
	if disc.BlockSize == 0 {
		disc.BlockSize = SectorData
	}
	// READ CAPACITY reports the last addressable block, not the count.
	disc.Sectors = last + 1
	return nil
}

// readTOC reads the table of contents in LBA form. The lead-out is reported
// as track 0xAA, and it is what gives the last real track its length.
func (d *Drive) readTOC(disc *Disc) error {
	head := make([]byte, 4)
	if err := d.in(tocCDB(0, len(head)), head, shortTimeout); err != nil {
		return err
	}
	total := be16(head[0:2]) + 2
	if total <= 4 {
		return nil
	}
	if total > 4096 {
		total = 4096
	}
	buf := make([]byte, total)
	if err := d.in(tocCDB(0, total), buf, shortTimeout); err != nil {
		return err
	}

	type entry struct {
		number int
		ctrl   byte
		lba    int64
	}
	var entries []entry
	for p := 4; p+8 <= len(buf); p += 8 {
		entries = append(entries, entry{
			number: int(buf[p+2]),
			ctrl:   buf[p+1] & 0x0f,
			lba:    int64(int32(be32(buf[p+4 : p+8]))),
		})
	}
	for i, e := range entries {
		if e.number == 0xaa { // lead-out: an end, not a track
			continue
		}
		end := int64(0)
		if i+1 < len(entries) {
			end = entries[i+1].lba
		}
		audio := e.ctrl&0x04 == 0
		t := Track{
			Number:        e.number,
			Audio:         audio,
			Start:         e.lba,
			Sectors:       end - e.lba,
			PreEmphasis:   audio && e.ctrl&0x01 != 0,
			FourChannel:   audio && e.ctrl&0x08 != 0,
			CopyPermitted: e.ctrl&0x02 != 0,
		}
		if t.Sectors < 0 {
			t.Sectors = 0
		}
		if audio {
			t.Bytes = t.Sectors * SectorRaw
			t.DurationSeconds = float64(t.Sectors) / FramesPerSecond
			disc.AudioTracks++
		} else {
			t.Bytes = t.Sectors * SectorData
			disc.DataTracks++
		}
		disc.Tracks = append(disc.Tracks, t)
	}
	return nil
}

func tocCDB(format byte, n int) []byte {
	// MSF bit clear: addresses come back as plain sector numbers.
	return []byte{opReadTOC, 0x00, format & 0x0f, 0, 0, 0, 0, byte(n >> 8), byte(n), 0}
}

// readATIP reads the pre-groove information pressed into a blank
// recordable CD: its manufacturer and how much will fit on it. It is the
// only thing a blank disc has to say about itself.
func (d *Drive) readATIP(disc *Disc) error {
	buf := make([]byte, 28)
	if err := d.in(tocCDB(4, len(buf)), buf, shortTimeout); err != nil {
		return err
	}
	if len(buf) < 16 {
		return nil
	}
	// Lead-out start in minutes/seconds/frames, at bytes 8..10.
	m, s, f := int64(buf[8]), int64(buf[9]), int64(buf[10])
	if m > 0 {
		disc.BlankSectors = (m*60+s)*FramesPerSecond + f - LeadInSectors
	}
	// The manufacturer is identified by the lead-in address, which is a
	// code rather than a place. Reporting the raw code is honest and still
	// useful: it is what disc databases are indexed by.
	disc.MediaID = fmt.Sprintf("%02d:%02d:%02d", buf[4], buf[5], buf[6])
	return nil
}

// readSessionInfo asks where the most recent session starts.
//
// It matters because a multi-session disc has a set of volume descriptors
// per session, and only the last one describes everything on the disc: its
// directory refers back to the files written earlier as well as to its own.
// Reading the descriptors at the start of the disc instead shows the first
// session and nothing that was added after it - which is what ripperX did
// until a disc with two sessions on it proved otherwise.
func (d *Drive) readSessionInfo(disc *Disc) error {
	buf := make([]byte, 12)
	// Format 0001b: session information.
	cdb := []byte{opReadTOC, 0x00, 0x01, 0, 0, 0, 0,
		byte(len(buf) >> 8), byte(len(buf)), 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return err
	}
	if len(buf) < 12 {
		return errors.New("READ TOC: short session answer")
	}
	start := int64(int32(be32(buf[8:12])))
	if start > 0 {
		disc.LastSessionStart = start
	}
	return nil
}

// readTrackInfo asks where the next session would go and how much room is
// left for it, by asking about the track that does not exist yet.
//
// Track 0xff means "the invisible track" and most drives answer it. The
// HL-DT-ST here refuses it outright, so the fallback is the last track of
// the last session, which READ DISC INFORMATION reports and which is that
// same not-yet-written track.
//
// Deriving the number from the table of contents instead does not work: on
// a DVD with three sessions on it this drive listed two tracks and knew
// about four, so counting the listed ones asked about a track that had
// already been written and got told there was no room - which read as a
// full disc.
func (d *Drive) readTrackInfo(disc *Disc) error {
	err := d.trackInfoInto(disc, 0xff)
	if !IsUnsupported(err) {
		return err
	}
	if disc.lastTrack <= 0 {
		return err
	}
	return d.trackInfoInto(disc, uint32(disc.lastTrack))
}

// trackInfoInto reads one track's information and takes from it what says
// whether the disc can still be written to.
func (d *Drive) trackInfoInto(disc *Disc, track uint32) error {
	buf := make([]byte, 36)
	cdb := []byte{opReadTrackInfo, 0x01, // addressed by track number
		byte(track >> 24), byte(track >> 16), byte(track >> 8), byte(track),
		0, byte(len(buf) >> 8), byte(len(buf)), 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return err
	}
	if len(buf) < 20 {
		return errors.New("READ TRACK INFORMATION: short answer")
	}
	// Bit 0 of byte 7 says whether the next writable address means
	// anything; it does not on a track that has already been written.
	if buf[7]&0x01 == 0 {
		return nil
	}
	disc.NextWritable = int64(be32(buf[12:16]))
	disc.WritableSectors = int64(be32(buf[16:20]))
	disc.WritableBytes = disc.WritableSectors * SectorData
	disc.Appendable = disc.WritableSectors > 0
	return nil
}

// MediaChanged reports whether the disc was swapped since the last call.
// GET EVENT STATUS NOTIFICATION asks without disturbing the drive, which is
// what makes it safe to poll while nothing else is happening.
func (d *Drive) MediaChanged() (changed bool, present bool, err error) {
	buf := make([]byte, 8)
	// Class 4: media. The polled bit makes this a question rather than a
	// subscription.
	cdb := []byte{opGetEventStatus, 0x01, 0, 0, 0x10, 0, 0, byte(len(buf) >> 8), byte(len(buf)), 0}
	if err := d.in(cdb, buf, shortTimeout); err != nil {
		return false, false, err
	}
	if len(buf) < 8 || buf[2]&0x07 != 4 {
		return false, false, nil
	}
	event := buf[4] & 0x0f
	present = buf[5]&0x02 != 0
	// 1 is eject requested, 2 new media, 3 media removed, 4 media changed.
	return event == 2 || event == 3 || event == 4, present, nil
}

// WaitReady spins until the drive says it has a disc it can read, or until
// the deadline. A drive handed a disc answers "becoming ready" for several
// seconds; treating that as a failure would make every rip started straight
// after loading a disc fail.
func (d *Drive) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := d.Ready()
		if err == nil {
			return nil
		}
		s := sense(err)
		becoming := s != nil && s.Key == 0x02 && s.ASC == 0x04
		if !becoming || time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}
