package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/Kseen715/ripperX/discfs"
)

// A DVD does not keep its films in its files. VTS_01_1.VOB and its siblings
// are a gigabyte apiece because the format says so, and what is inside them
// is several titles one after another, each with its own timeline starting
// at zero again. A disc of eighteen episodes is five files; a film with a
// trailer on it is one file holding two timelines.
//
// That is why the length of a VOB cannot be asked of the VOB. ffprobe
// answers honestly - the last timestamp minus the first - and on such a
// disc that is the length of whichever timeline happened to be last, which
// is how a two-hour disc comes back as eighteen seconds.
//
// The disc does record all of it, in the IFO files beside the VOBs: which
// titles exist, how long each one plays for, and which sectors of the VOB
// set it occupies. So they are read, and a title - not a file - is what
// ripperX offers to play. A film disc has one of them, the length of the
// film; this disc has eighteen.

const dvdSectorSize = 2048

// dvdTitle is one thing a player would call a title: a length, and the
// stretch of the VOB set it is stored in.
type dvdTitle struct {
	Number   int     `json:"number" doc:"the title's number on the disc, as a player would show it"`
	VTS      int     `json:"vts" doc:"which video title set holds it"`
	Seconds  float64 `json:"seconds" doc:"how long it plays for, as the disc records it"`
	Chapters int     `json:"chapters" doc:"how many chapters it is divided into"`
	Angles   int     `json:"angles" doc:"how many angles it was authored with"`
	Bytes    int64   `json:"bytes" doc:"how much of the disc it occupies"`

	// where it lives in the VTS's VOB set, as byte ranges in the
	// concatenation of VTS_XX_1.VOB onwards. A title is usually one run;
	// several cells make several.
	ranges []byteRange
}

type byteRange struct{ off, size int64 }

// dvdDisc is what the IFOs say about this disc.
type dvdDisc struct {
	Titles []dvdTitle `json:"titles" doc:"every title on the disc, in the order a player would list them"`
}

var errNotDVDVideo = errors.New("this disc is not a DVD-Video")

// readDVD reads the disc's own index. It costs a handful of small reads -
// the IFOs are kilobytes - and it is the only way to be right about what is
// on a video disc.
func readDVD(fsys discfs.FS) (*dvdDisc, error) {
	vmg, err := readIFO(fsys, "/VIDEO_TS/VIDEO_TS.IFO", "DVDVIDEO-VMG")
	if err != nil {
		return nil, err
	}
	entries, err := titleTable(vmg)
	if err != nil {
		return nil, err
	}

	sets := map[int]*vtsIndex{}
	disc := &dvdDisc{}
	for i, e := range entries {
		set, ok := sets[e.vts]
		if !ok {
			raw, err := readIFO(fsys, fmt.Sprintf("/VIDEO_TS/VTS_%02d_0.IFO", e.vts), "DVDVIDEO-VTS")
			if err != nil {
				// A title set that cannot be read costs its titles, not the
				// whole disc: the others are still watchable.
				continue
			}
			set, err = readVTS(raw)
			if err != nil {
				continue
			}
			sets[e.vts] = set
		}
		pgc, err := set.titlePGC(e.titleInVTS)
		if err != nil {
			continue
		}
		title := dvdTitle{
			Number:   i + 1,
			VTS:      e.vts,
			Seconds:  pgc.seconds,
			Chapters: e.chapters,
			Angles:   e.angles,
			ranges:   pgc.ranges,
		}
		for _, r := range pgc.ranges {
			title.Bytes += r.size
		}
		disc.Titles = append(disc.Titles, title)
	}
	if len(disc.Titles) == 0 {
		return nil, errors.New("this disc's index names no titles this server can read")
	}
	return disc, nil
}

// readIFO reads one index file whole and checks it says what it should. An
// IFO is measured in kilobytes, so reading all of it is the cheap way.
func readIFO(fsys discfs.FS, path, magic string) ([]byte, error) {
	f, entry, err := fsys.Open(path)
	if err != nil {
		return nil, errNotDVDVideo
	}
	if entry.Size < dvdSectorSize || entry.Size > 8<<20 {
		return nil, errNotDVDVideo
	}
	raw := make([]byte, entry.Size)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if string(raw[:len(magic)]) != magic {
		return nil, errNotDVDVideo
	}
	return raw, nil
}

// ttEntry is one line of the disc's title table: which title set holds the
// title, and which title it is within it.
type ttEntry struct {
	vts        int
	titleInVTS int
	chapters   int
	angles     int
}

// titleTable reads VMG_TT_SRPT, the list of titles the disc presents.
func titleTable(vmg []byte) ([]ttEntry, error) {
	base, err := sectorOffset(vmg, 0xC4)
	if err != nil {
		return nil, err
	}
	count, err := be16at(vmg, base)
	if err != nil {
		return nil, err
	}
	var out []ttEntry
	for i := range int(count) {
		off := base + 8 + i*12
		if off+12 > len(vmg) {
			break
		}
		out = append(out, ttEntry{
			angles:     int(vmg[off+1]),
			chapters:   int(binary.BigEndian.Uint16(vmg[off+2 : off+4])),
			vts:        int(vmg[off+6]),
			titleInVTS: int(vmg[off+7]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("this disc's title table is empty")
	}
	return out, nil
}

// vtsIndex is one title set's own index: its program chains, and which of
// them each of its titles begins in.
type vtsIndex struct {
	pgcs    []dvdPGC
	byTitle []int // title within the set, in order, to program chain number
}

// dvdPGC is a program chain: the thing that actually has a length and a
// place on the disc.
type dvdPGC struct {
	seconds float64
	ranges  []byteRange
}

func readVTS(raw []byte) (*vtsIndex, error) {
	idx := &vtsIndex{}

	pgciBase, err := sectorOffset(raw, 0xCC)
	if err != nil {
		return nil, err
	}
	count, err := be16at(raw, pgciBase)
	if err != nil {
		return nil, err
	}
	for i := range int(count) {
		off := pgciBase + 8 + i*8
		if off+8 > len(raw) {
			break
		}
		start := int(binary.BigEndian.Uint32(raw[off+4 : off+8]))
		pgc, err := readPGC(raw, pgciBase+start)
		if err != nil {
			return nil, err
		}
		idx.pgcs = append(idx.pgcs, pgc)
	}
	if len(idx.pgcs) == 0 {
		return nil, errors.New("this title set has no program chains")
	}

	// VTS_PTT_SRPT says which chain a title starts in. Without it the
	// titles are taken to be the chains in order, which is what they are on
	// every disc that has one title per chain.
	if ptt, err := sectorOffset(raw, 0xC8); err == nil {
		if n, err := be16at(raw, ptt); err == nil {
			for i := range int(n) {
				off := ptt + 8 + i*4
				if off+4 > len(raw) {
					break
				}
				first := ptt + int(binary.BigEndian.Uint32(raw[off:off+4]))
				if first+2 > len(raw) {
					break
				}
				idx.byTitle = append(idx.byTitle, int(binary.BigEndian.Uint16(raw[first:first+2])))
			}
		}
	}
	return idx, nil
}

// titlePGC is the chain a title of this set plays.
func (v *vtsIndex) titlePGC(title int) (dvdPGC, error) {
	n := title
	if title >= 1 && title <= len(v.byTitle) {
		n = v.byTitle[title-1]
	}
	if n < 1 || n > len(v.pgcs) {
		return dvdPGC{}, fmt.Errorf("title %d names program chain %d, which this title set does not have", title, n)
	}
	return v.pgcs[n-1], nil
}

// readPGC reads one program chain: how long it plays for, and the sectors
// of the VOB set its cells occupy.
func readPGC(raw []byte, at int) (dvdPGC, error) {
	if at < 0 || at+0xEC > len(raw) {
		return dvdPGC{}, errors.New("a program chain lies outside the index")
	}
	cells := int(raw[at+3])
	pgc := dvdPGC{seconds: dvdTime(raw[at+4 : at+8])}

	playback := at + int(binary.BigEndian.Uint16(raw[at+0xE8:at+0xEA]))
	for i := range cells {
		off := playback + i*24
		if off+24 > len(raw) {
			break
		}
		first := int64(binary.BigEndian.Uint32(raw[off+8 : off+12]))
		last := int64(binary.BigEndian.Uint32(raw[off+20 : off+24]))
		if last < first {
			continue
		}
		pgc.ranges = append(pgc.ranges, byteRange{
			off:  first * dvdSectorSize,
			size: (last - first + 1) * dvdSectorSize,
		})
	}
	if len(pgc.ranges) == 0 {
		return dvdPGC{}, errors.New("a program chain names no sectors")
	}
	// Cells are usually written in the order they are played, but nothing
	// promises it, and a reader that seeks backwards through a disc is a
	// reader that takes minutes over what should take seconds.
	sort.Slice(pgc.ranges, func(i, j int) bool { return pgc.ranges[i].off < pgc.ranges[j].off })
	return pgc, nil
}

// dvdTime is how a DVD writes a duration: four bytes of binary-coded
// decimal - hours, minutes, seconds, frames - with the frame rate in the
// top two bits of the last one.
func dvdTime(b []byte) float64 {
	bcd := func(v byte) float64 { return float64(v>>4)*10 + float64(v&0x0f) }
	fps := map[byte]float64{1: 25, 3: 30000.0 / 1001.0}[b[3]>>6]
	secs := bcd(b[0])*3600 + bcd(b[1])*60 + bcd(b[2])
	if fps > 0 {
		secs += bcd(b[3]&0x3f) / fps
	}
	return secs
}

// sectorOffset reads one of the index's pointers, which are all sector
// numbers relative to the file they are in.
func sectorOffset(raw []byte, at int) (int, error) {
	if at+4 > len(raw) {
		return 0, errNotDVDVideo
	}
	off := int(binary.BigEndian.Uint32(raw[at:at+4])) * dvdSectorSize
	if off <= 0 || off+8 > len(raw) {
		return 0, fmt.Errorf("this disc's index points outside itself")
	}
	return off, nil
}

func be16at(raw []byte, at int) (uint16, error) {
	if at+2 > len(raw) {
		return 0, errNotDVDVideo
	}
	return binary.BigEndian.Uint16(raw[at : at+2]), nil
}

// openDVDTitle opens one title as a single stream: the VOB set of its title
// set, cut down to the ranges the title occupies. What comes back is
// seekable, so ffmpeg can be asked for the segment an hour in and read only
// that.
func openDVDTitle(fsys discfs.FS, title dvdTitle) (discfs.File, int64, error) {
	first := fmt.Sprintf("/VIDEO_TS/VTS_%02d_1.VOB", title.VTS)
	parts, err := titleParts(fsys, first)
	if err != nil {
		return nil, 0, err
	}
	whole, err := openTitle(fsys, parts)
	if err != nil {
		return nil, 0, err
	}
	total := titleSize(parts)

	files := make([]discfs.File, 0, len(title.ranges))
	sizes := make([]int64, 0, len(title.ranges))
	for _, r := range title.ranges {
		if r.off >= total {
			continue
		}
		size := min(r.size, total-r.off)
		files = append(files, io.NewSectionReader(whole, r.off, size))
		sizes = append(sizes, size)
	}
	if len(files) == 0 {
		return nil, 0, errors.New("this title's sectors are not on the disc")
	}
	if len(files) == 1 {
		return files[0], sizes[0], nil
	}
	mf := newMultiFile(files, sizes)
	return mf, mf.size, nil
}

// handleTitles lists what is actually on a video disc. It is the video
// answer to the list of audio tracks: a disc's files are not its films, and
// this is where that is resolved.
func (s *server) handleTitles(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	disc, err := d.dvd()
	if err != nil {
		switch {
		case driveUnavailable(err):
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		case errors.Is(err, errNotDVDVideo):
			writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		default:
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, disc)
}
