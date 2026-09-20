package main

import (
	"encoding/binary"
	"testing"
)

// A DVD's index, built the way a disc has it: two titles, each its own
// program chain, each one cell somewhere in the VOB set. The numbers here
// are the ones a real disc gave - seven minutes nine seconds, and six
// thirty-six - because those are what a two-hour disc of episodes looks
// like, and what ffprobe cannot be asked for.
func dvdIFOs() (vmg, vts []byte) {
	sector := func(n int) int { return n * dvdSectorSize }
	vmg = make([]byte, sector(4))
	copy(vmg, "DVDVIDEO-VMG")
	binary.BigEndian.PutUint32(vmg[0xC4:], 2) // VMG_TT_SRPT is in sector 2
	tt := sector(2)
	binary.BigEndian.PutUint16(vmg[tt:], 2) // two titles
	for i, e := range []struct{ vts, ttn, chapters, angles int }{{1, 1, 3, 1}, {1, 2, 1, 1}} {
		off := tt + 8 + i*12
		vmg[off+1] = byte(e.angles)
		binary.BigEndian.PutUint16(vmg[off+2:], uint16(e.chapters))
		vmg[off+6] = byte(e.vts)
		vmg[off+7] = byte(e.ttn)
	}

	vts = make([]byte, sector(8))
	copy(vts, "DVDVIDEO-VTS")
	binary.BigEndian.PutUint32(vts[0xC8:], 1) // VTS_PTT_SRPT
	binary.BigEndian.PutUint32(vts[0xCC:], 2) // VTS_PGCI
	ptt := sector(1)
	binary.BigEndian.PutUint16(vts[ptt:], 2)
	// The two titles play chains two and one, in that order: a disc is
	// under no obligation to number them the same way, and a reader that
	// assumes it does plays the wrong episode.
	for i, chain := range []int{2, 1} {
		binary.BigEndian.PutUint32(vts[ptt+8+i*4:], uint32(16+i*4))
		binary.BigEndian.PutUint16(vts[ptt+16+i*4:], uint16(chain))
	}

	pgci := sector(2)
	binary.BigEndian.PutUint16(vts[pgci:], 2)
	chains := []struct {
		at          int
		time        [4]byte
		first, last uint32
	}{
		// 00:06:36 and 00:07:09, both at 25 frames a second.
		{at: 0x200, time: [4]byte{0x00, 0x06, 0x36, 1 << 6}, first: 0, last: 9},
		{at: 0x800, time: [4]byte{0x00, 0x07, 0x09, 1 << 6}, first: 10, last: 24},
	}
	for i, c := range chains {
		binary.BigEndian.PutUint32(vts[pgci+8+i*8+4:], uint32(c.at))
		pgc := pgci + c.at
		vts[pgc+3] = 1 // one cell
		copy(vts[pgc+4:], c.time[:])
		binary.BigEndian.PutUint16(vts[pgc+0xE8:], 0x100) // where its cells are
		cell := pgc + 0x100
		binary.BigEndian.PutUint32(vts[cell+8:], c.first)
		binary.BigEndian.PutUint32(vts[cell+20:], c.last)
	}
	return vmg, vts
}

func ifoFS() fakeFS {
	vmg, vts := dvdIFOs()
	f := dvd()
	f.files["/VIDEO_TS/VIDEO_TS.IFO"] = vmg
	f.files["/VIDEO_TS/VTS_01_0.IFO"] = vts
	return f
}

func TestReadDVDTakesTheTitlesFromTheIndex(t *testing.T) {
	disc, err := readDVD(ifoFS())
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Titles) != 2 {
		t.Fatalf("the disc has %d titles, want 2", len(disc.Titles))
	}

	// Title one plays chain two: seven minutes nine seconds, from sector 10
	// to sector 24 inclusive.
	one := disc.Titles[0]
	if one.Number != 1 || one.VTS != 1 || one.Chapters != 3 {
		t.Errorf("title one is %+v, want number 1 of vts 1 with 3 chapters", one)
	}
	if one.Seconds != 429 {
		t.Errorf("title one plays for %.2f seconds, want 429", one.Seconds)
	}
	if want := int64(15 * dvdSectorSize); one.Bytes != want {
		t.Errorf("title one is %d bytes, want %d", one.Bytes, want)
	}
	if len(one.ranges) != 1 || one.ranges[0].off != 10*dvdSectorSize {
		t.Errorf("title one is at %+v, want one run starting at sector 10", one.ranges)
	}

	two := disc.Titles[1]
	if two.Seconds != 396 {
		t.Errorf("title two plays for %.2f seconds, want 396", two.Seconds)
	}
	if two.ranges[0].off != 0 {
		t.Errorf("title two starts at byte %d, want 0", two.ranges[0].off)
	}
}

// A disc that is not a DVD-Video says so plainly, because that answer is
// remembered and shown: an audio CD must not come back as a broken DVD.
func TestReadDVDRefusesEverythingElse(t *testing.T) {
	if _, err := readDVD(dvd()); err == nil {
		t.Fatal("a disc with no index came back as a DVD")
	} else if err != errNotDVDVideo {
		t.Errorf("a disc with no index gave %v, want %v", err, errNotDVDVideo)
	}

	f := ifoFS()
	f.files["/VIDEO_TS/VIDEO_TS.IFO"] = []byte("this is not an IFO at all, but it is long enough to read" +
		string(make([]byte, dvdSectorSize)))
	if _, err := readDVD(f); err != errNotDVDVideo {
		t.Errorf("a file that is not an index gave %v, want %v", err, errNotDVDVideo)
	}
}

func TestDVDTimeReadsBinaryCodedDecimal(t *testing.T) {
	for _, tc := range []struct {
		b    [4]byte
		want float64
	}{
		{[4]byte{0x01, 0x23, 0x45, 1 << 6}, 3600 + 23*60 + 45},  // 01:23:45 at 25 fps
		{[4]byte{0x00, 0x07, 0x09, 1<<6 | 0x12}, 429 + 12.0/25}, // and twelve frames
		{[4]byte{0x00, 0x00, 0x30, 3 << 6}, 30},                 // 30 seconds at 29.97
	} {
		if got := dvdTime(tc.b[:]); got != tc.want {
			t.Errorf("dvdTime(%x) = %v, want %v", tc.b, got, tc.want)
		}
	}
}
