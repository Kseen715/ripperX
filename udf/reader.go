package udf

import (
	"errors"
	"io"
	"sync"

	"github.com/Kseen715/ripperX/discfs"
)

// A UDF file is not necessarily one run of blocks. It can be scattered
// across the disc in pieces, and it can have holes in it - space that was
// allocated and never written, which reads back as zeroes because that is
// what it is. An ISO 9660 file is always one run, which is why that reader
// can hand out an io.SectionReader and this one cannot.
//
// So a file is read through a small map: where each piece starts in the
// file, where it is on the disc, and how long it is. A read that crosses a
// join is served from both pieces, and nothing above here has to know.

type piece struct {
	at  int64 // offset within the file
	ex  extent
	end int64
}

type fileReader struct {
	fs     *FS
	size   int64
	pieces []piece

	mu  sync.Mutex
	pos int64

	// inline is the whole file, for one small enough to be kept inside its
	// own directory entry.
	inline []byte
}

func (f *FS) reader(e *fileEntry) discfs.File {
	r := &fileReader{fs: f, size: e.size, inline: e.inline}
	var at int64
	for _, ex := range e.extents {
		if at >= e.size {
			break
		}
		length := ex.length
		if at+length > e.size {
			length = e.size - at
		}
		r.pieces = append(r.pieces, piece{at: at, ex: extent{
			block: ex.block, length: length, recorded: ex.recorded}, end: at + length})
		at += length
	}
	return r
}

func (r *fileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if r.inline != nil {
		n := copy(p, r.inline[min(off, int64(len(r.inline))):])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	read := 0
	for read < len(p) {
		at := off + int64(read)
		if at >= r.size {
			return read, io.EOF
		}
		pc := r.pieceAt(at)
		if pc == nil {
			// A gap in the map rather than in the file: the entry claimed
			// more bytes than its extents describe. Zeroes are the honest
			// answer, and the length stays what the disc said.
			p[read] = 0
			read++
			continue
		}
		within := at - pc.at
		want := int64(len(p) - read)
		if avail := pc.end - at; want > avail {
			want = avail
		}
		dst := p[read : read+int(want)]
		if !pc.ex.recorded {
			for i := range dst {
				dst[i] = 0
			}
			read += int(want)
			continue
		}
		n, err := r.fs.r.ReadAt(dst, r.fs.byteAt(pc.ex.block)+within)
		read += n
		if err != nil && !errors.Is(err, io.EOF) {
			return read, err
		}
		if n == 0 {
			return read, io.ErrUnexpectedEOF
		}
	}
	return read, nil
}

func (r *fileReader) pieceAt(off int64) *piece {
	for i := range r.pieces {
		if off >= r.pieces[i].at && off < r.pieces[i].end {
			return &r.pieces[i]
		}
	}
	return nil
}

func (r *fileReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.ReadAt(p, r.pos)
	r.pos += int64(n)
	return n, err
}

func (r *fileReader) Seek(off int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.size
	default:
		return 0, errors.New("bad whence")
	}
	if off < 0 {
		return 0, errors.New("seek before the start of the file")
	}
	r.pos = off
	return off, nil
}
