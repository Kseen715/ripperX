package udf

import (
	"bytes"
	"testing"
)

// The volume every test in this package reads, and the pieces it is built
// from.
//
// A small UDF volume, assembled by hand. Building one here rather than
// checking in a binary means every field is visible beside the reader that
// parses it.
//
//	 32  primary volume descriptor
//	 33  partition descriptor: the partition starts at 64
//	 34  logical volume descriptor: the file set is at partition block 0
//	 35  terminating descriptor
//	256  anchor, pointing at the sequence above
//
// and inside the partition, counted from block 64:
//
//	0  file set descriptor, naming the root's ICB
//	1  root directory entry
//	2  the root's contents: the parent, a file, a directory
//	3  HELLO.TXT's entry            4  its contents
//	5  the directory's entry        6  its contents
//	7  split.bin's entry, in two pieces
//	8  the first piece              9  the second
//	10 a Cyrillic name's entry, with its contents inside it
const (
	volBlocks = 320
	seqAt     = 32
	partAt    = 64
	partLen   = 64

	helloText = "hello, disc\n"
	firstHalf = "the first piece of a file that is not "
	lastHalf  = "all in one place on the disc\n"
	// A name in Cyrillic, which UDF records as sixteen-bit characters and
	// ISO 9660 cannot carry at all outside Joliet.
	cyrillicName = "Наклейка.txt"
	cyrillicText = "внутри\n"
)

type build struct {
	img []byte
	// extended makes every file entry an Extended File Entry, the shape UDF
	// 2.00 and later write, rather than the plain one a DVD-Video carries.
	extended bool
}

func (b *build) block(n int64) []byte { return b.img[n*SectorSize : (n+1)*SectorSize] }

// part addresses a block inside the partition, which is how every
// descriptor in the volume refers to one.
func (b *build) part(n int64) []byte { return b.block(partAt + n) }

// tag writes a descriptor tag. The checksum is what the reader uses to tell
// a descriptor from a sector of rubbish, and the location is what tells a
// descriptor read from the right place from one read from the wrong place.
func tag(b []byte, id int, at int64) {
	putLE16(b[0:2], uint16(id))
	putLE16(b[2:4], 2) // descriptor version
	putLE16(b[6:8], 1) // serial number
	putLE32(b[12:16], uint32(at))
	var sum byte
	for i, c := range b[:16] {
		if i == 4 {
			continue
		}
		sum += c
	}
	b[4] = sum
}

// shortAD and longAD write the two kinds of allocation descriptor.
func putShortAD(b []byte, block, length int64) {
	putLE32(b[0:4], uint32(length))
	putLE32(b[4:8], uint32(block))
}

func putLongAD(b []byte, block, length int64) {
	putLE32(b[0:4], uint32(length))
	putLE32(b[4:8], uint32(block))
}

// putDString writes an identifier the way UDF does: the compression byte,
// the characters, and the length in the last byte of the field.
func putDString(b []byte, s string) {
	for i := range b {
		b[i] = 0
	}
	b[0] = 8
	n := copy(b[1:len(b)-1], s)
	b[len(b)-1] = byte(n + 1)
}

// fileEntryAt writes a file or directory entry: what it is, how big, and
// where its contents are.
func (b *build) fileEntryAt(at int64, fileType byte, size int64, extents [][2]int64, inline []byte) {
	e := b.part(at)
	id, adOff, sizeOff, timeOff := tagFileEntry, int64(176), 56, 84
	if b.extended {
		id, adOff, sizeOff, timeOff = tagExtendedFileEntry, 216, 56, 92
	}
	tag(e, id, partAt+at)
	e[16+11] = fileType
	putLE32(e[44:48], 0o444<<5) // readable by everyone
	putLE64(e[sizeOff:sizeOff+8], uint64(size))
	// 25 January 2013, which is when the disc that prompted all this was
	// written.
	putLE16(e[timeOff:timeOff+2], 0x1000) // local time, zero minutes from UTC
	putLE16(e[timeOff+2:timeOff+4], 2013)
	e[timeOff+4], e[timeOff+5] = 1, 25
	e[timeOff+6], e[timeOff+7], e[timeOff+8] = 15, 28, 52

	if inline != nil {
		putLE16(e[16+18:16+20], 3) // the contents are in the entry itself
		putLE32(e[adOff-4:adOff], uint32(len(inline)))
		copy(e[adOff:], inline)
		return
	}
	putLE16(e[16+18:16+20], 0) // short allocation descriptors
	putLE32(e[adOff-4:adOff], uint32(len(extents)*8))
	for i, ex := range extents {
		putShortAD(e[adOff+int64(i)*8:], ex[0], ex[1])
	}
}

// fid writes one file identifier descriptor and returns how long it was, so
// the next one can go after it.
func fid(dst []byte, at int64, name string, icb int64, isDir, parent bool) int {
	tag(dst, tagFileIdentifier, at)
	putLE16(dst[16:18], 1) // file version
	if isDir {
		dst[18] |= 0x02
	}
	if parent {
		dst[18] |= 0x08
	}
	putLongAD(dst[20:36], icb, SectorSize)

	var encoded []byte
	if name != "" {
		if isASCII(name) {
			encoded = append([]byte{8}, name...)
		} else {
			// Sixteen-bit characters, big-endian, which is how a name that
			// is not Latin is recorded.
			encoded = []byte{16}
			for _, r := range name {
				encoded = append(encoded, byte(r>>8), byte(r))
			}
		}
	}
	dst[19] = byte(len(encoded))
	copy(dst[38:], encoded)
	return roundUp4(38 + len(encoded))
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 0x7e {
			return false
		}
	}
	return true
}

func buildVolume(t *testing.T, extended bool) []byte {
	t.Helper()
	b := &build{img: make([]byte, volBlocks*SectorSize), extended: extended}

	// --- the anchor, and the sequence it points at ---
	anchor := b.block(256)
	tag(anchor, tagAnchorPointer, 256)
	putLE32(anchor[16:20], 4*SectorSize)
	putLE32(anchor[20:24], seqAt)

	pvd := b.block(seqAt)
	tag(pvd, tagPrimaryVolume, seqAt)
	putDString(pvd[24:56], "TESTDISC")
	putDString(pvd[72:200], "TESTSET")

	pd := b.block(seqAt + 1)
	tag(pd, tagPartition, seqAt+1)
	putLE32(pd[188:192], partAt)
	putLE32(pd[192:196], partLen)

	lvd := b.block(seqAt + 2)
	tag(lvd, tagLogicalVolume, seqAt+2)
	putLE32(lvd[212:216], SectorSize)
	putLongAD(lvd[248:264], 0, SectorSize) // the file set, at partition block 0

	term := b.block(seqAt + 3)
	tag(term, tagTerminating, seqAt+3)

	// --- the file set, which names the root ---
	fsd := b.part(0)
	tag(fsd, tagFileSet, partAt)
	putDString(fsd[304:336], "TESTDISC")
	putLongAD(fsd[400:416], 1, SectorSize)

	// --- the root directory ---
	root := b.part(2)
	off := fid(root, partAt+2, "", 1, true, true)
	off += fid(root[off:], partAt+2, "HELLO.TXT", 3, false, false)
	off += fid(root[off:], partAt+2, "sub", 5, true, false)
	off += fid(root[off:], partAt+2, cyrillicName, 10, false, false)
	b.fileEntryAt(1, fileTypeDirectory, int64(off), [][2]int64{{2, int64(off)}}, nil)

	b.fileEntryAt(3, 5, int64(len(helloText)), [][2]int64{{4, int64(len(helloText))}}, nil)
	copy(b.part(4), helloText)

	// --- a subdirectory, holding a file in two pieces ---
	sub := b.part(6)
	off = fid(sub, partAt+6, "", 1, true, true)
	off += fid(sub[off:], partAt+6, "split.bin", 7, false, false)
	b.fileEntryAt(5, fileTypeDirectory, int64(off), [][2]int64{{6, int64(off)}}, nil)

	whole := int64(len(firstHalf) + len(lastHalf))
	b.fileEntryAt(7, 5, whole, [][2]int64{
		{8, int64(len(firstHalf))},
		{9, int64(len(lastHalf))},
	}, nil)
	copy(b.part(8), firstHalf)
	copy(b.part(9), lastHalf)

	// --- a file small enough to live inside its own entry ---
	b.fileEntryAt(10, 5, int64(len(cyrillicText)), nil, []byte(cyrillicText))
	return b.img
}

func openTest(t *testing.T, extended bool) *FS {
	t.Helper()
	fs, err := Open(bytes.NewReader(buildVolume(t, extended)), volBlocks)
	if err != nil {
		t.Fatalf("opening the test volume: %v", err)
	}
	return fs
}

func putLE16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putLE64(b []byte, v uint64) {
	putLE32(b[0:4], uint32(v))
	putLE32(b[4:8], uint32(v>>32))
}
