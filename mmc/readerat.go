package mmc

import (
	"fmt"
	"io"
	"sync"
)

// DataReader presents the drive's 2048-byte data sectors as a plain byte
// range, so the filesystem reader and an HTTP Range request can both work
// in offsets without knowing anything about sectors.
//
// Reads are rounded out to whole chunks and the last few chunks are kept,
// because walking a directory tree reads the same handful of sectors over
// and over - and on an optical drive every one of those is a seek measured
// in tens of milliseconds. The cache is small and fixed: it exists to make
// browsing a disc bearable, not to hold a copy of it.
type DataReader struct {
	d       *Drive
	sectors int64
	chunk   int

	mu     sync.Mutex
	cache  map[int64][]byte
	recent []int64
}

const cachedChunks = 16

// DataReaderAt returns a reader over the first sectors sectors of the disc.
func (d *Drive) DataReaderAt(sectors int64) *DataReader {
	return &DataReader{
		d:       d,
		sectors: sectors,
		chunk:   ChunkData,
		cache:   make(map[int64][]byte, cachedChunks),
	}
}

// Size is the length of the readable range in bytes.
func (r *DataReader) Size() int64 { return r.sectors * SectorData }

func (r *DataReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	total := r.Size()
	if off >= total {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off+int64(n) < total {
		chunkLBA := (off + int64(n)) / (int64(r.chunk) * SectorData) * int64(r.chunk)
		data, err := r.chunkAt(chunkLBA)
		if err != nil {
			return n, err
		}
		within := (off + int64(n)) - chunkLBA*SectorData
		if within >= int64(len(data)) {
			return n, io.EOF
		}
		copied := copy(p[n:], data[within:])
		n += copied
		if copied == 0 {
			break
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunkAt returns the cached chunk starting at that sector, reading it if
// this is the first time it has been asked for.
func (r *DataReader) chunkAt(lba int64) ([]byte, error) {
	r.mu.Lock()
	if data, ok := r.cache[lba]; ok {
		r.mu.Unlock()
		return data, nil
	}
	r.mu.Unlock()

	count := r.chunk
	if rest := r.sectors - lba; rest < int64(count) {
		count = int(rest)
	}
	if count <= 0 {
		return nil, io.EOF
	}
	buf := make([]byte, count*SectorData)
	// A bad sector inside a directory or a file being streamed is reported
	// as zeroes rather than failing the whole read: the rest of the disc is
	// still worth having, and a rip records where the holes were.
	if _, err := r.d.ReadDataRetry(lba, count, buf); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[lba] = buf
	r.recent = append(r.recent, lba)
	for len(r.recent) > cachedChunks {
		delete(r.cache, r.recent[0])
		r.recent = r.recent[1:]
	}
	return buf, nil
}
