package iso9660

import (
	"bytes"
	"testing"
)

// The volume every test in this package reads, and the pieces it is built
// from. Helpers only one test file needs stay in that file.
//
// A small ISO 9660 volume, assembled by hand. Building one here rather than
// checking in a binary means the test says what each field is for, and a
// change to the reader can be tried against a layout that is written out in
// full below.
//
//	16  primary volume descriptor
//	17  terminator
//	18  root directory
//	19  the SUB directory
//	20  HELLO.TXT's contents
const (
	lbaPVD  = 16
	lbaTerm = 17
	lbaRoot = 18
	lbaSub  = 19
	lbaFile = 20

	helloText = "hello, disc\n"
	// The long name lives in a Rock Ridge NM entry; the ISO name beside it
	// is the 8.3 one a reader without Rock Ridge would show.
	longName = "a long file name.txt"
)

// both writes a number in the "both byte orders" form the format uses
// everywhere: little-endian, then big-endian.
func both32(b []byte, v uint32) {
	putLE32(b[0:4], v)
	b[4], b[5], b[6], b[7] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// dirRecord assembles one directory record.
func dirRecord(name string, extent, size uint32, isDir bool, susp []byte) []byte {
	nameLen := len(name)
	n := 33 + nameLen
	if nameLen%2 == 0 {
		n++ // the pad byte that keeps the next field even-aligned
	}
	rec := make([]byte, n+len(susp))
	rec[0] = byte(len(rec))
	both32(rec[2:10], extent)
	both32(rec[10:18], size)
	rec[18], rec[19], rec[20] = 125, 6, 15 // 2025-06-15
	rec[21], rec[22], rec[23] = 12, 0, 0
	if isDir {
		rec[25] = 0x02
	}
	rec[28], rec[29], rec[30], rec[31] = 1, 0, 0, 1 // volume sequence number
	rec[32] = byte(nameLen)
	copy(rec[33:], name)
	copy(rec[n:], susp)
	return rec
}

// nmEntry is the Rock Ridge alternate name: the real, long file name.
func nmEntry(name string) []byte {
	e := make([]byte, 5+len(name))
	e[0], e[1] = 'N', 'M'
	e[2] = byte(len(e))
	e[3] = 1 // version
	e[4] = 0 // no continuation
	copy(e[5:], name)
	return e
}

func pad(s string, n int) []byte {
	b := bytes.Repeat([]byte{' '}, n)
	copy(b, s)
	return b
}

func buildISO(t *testing.T) []byte {
	t.Helper()
	img := make([]byte, 24*BlockSize)
	sector := func(n int) []byte { return img[n*BlockSize : (n+1)*BlockSize] }

	// --- primary volume descriptor ---
	pvd := sector(lbaPVD)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], pad("RIPPERX-TEST", 32))
	copy(pvd[40:72], pad("TEST VOLUME", 32))
	both32(pvd[80:88], 24) // the volume is 24 sectors long
	copy(pvd[156:190], dirRecord("\x00", lbaRoot, BlockSize, true, nil))
	copy(pvd[190:318], pad("SET", 128))
	copy(pvd[318:446], pad("A PUBLISHER", 128))
	copy(pvd[446:574], pad("A PREPARER", 128))
	copy(pvd[574:702], pad("AN APPLICATION", 128))
	copy(pvd[813:830], []byte("2025061512000000\x00"))
	copy(pvd[830:847], []byte("2025061512000000\x00"))

	// --- terminator ---
	term := sector(lbaTerm)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1

	// --- root directory ---
	root := sector(lbaRoot)
	off := 0
	put := func(dst []byte, rec []byte) {
		copy(dst[off:], rec)
		off += len(rec)
	}
	put(root, dirRecord("\x00", lbaRoot, BlockSize, true, nil))
	put(root, dirRecord("\x01", lbaRoot, BlockSize, true, nil))
	put(root, dirRecord("HELLO.TXT;1", lbaFile, uint32(len(helloText)), false, nil))
	put(root, dirRecord("SUB", lbaSub, BlockSize, true, nil))

	// --- the SUB directory, whose one file carries a Rock Ridge name ---
	sub := sector(lbaSub)
	off = 0
	put(sub, dirRecord("\x00", lbaSub, BlockSize, true, nil))
	put(sub, dirRecord("\x01", lbaRoot, BlockSize, true, nil))
	put(sub, dirRecord("LONG.TXT;1", lbaFile, uint32(len(helloText)), false, nmEntry(longName)))

	copy(sector(lbaFile), helloText)
	return img
}

func open(t *testing.T) *FS {
	t.Helper()
	fsys, err := Open(bytes.NewReader(buildISO(t)))
	if err != nil {
		t.Fatalf("opening the test volume: %v", err)
	}
	return fsys
}
