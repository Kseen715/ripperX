// Fixtures and helpers used by more than one test file in this package.
// Anything needed by a single file stays in that file, where it can be
// read beside the thing it is for.

package main

import (
	"bytes"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
)

func testServer(t *testing.T) *server {
	t.Helper()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		web: sub, authOn: true, drives: newDriveSet(nil),
		selfHost: "127.0.0.1:8998", nonces: newNonceStore(),
	}
	s.jobs = newJobManager(s)
	s.api = s.routes(&auth{})
	return s
}

// testVolume builds the smallest ISO 9660 volume that has a file in it, so
// the archive and rip paths can be exercised without a disc. It is
// deliberately minimal - a root directory and two files, no Joliet and no
// Rock Ridge; the reader itself is tested thoroughly in its own package.
func testVolume(t *testing.T) *iso9660.FS {
	t.Helper()
	const (
		lbaPVD  = 16
		lbaRoot = 18
		lbaData = 19
	)
	const body = "hello, disc\n"

	img := make([]byte, 24*iso9660.BlockSize)
	sector := func(n int) []byte { return img[n*iso9660.BlockSize : (n+1)*iso9660.BlockSize] }

	both := func(b []byte, v uint32) {
		b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		b[4], b[5], b[6], b[7] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	}
	record := func(name string, extent, size uint32, isDir bool) []byte {
		n := 33 + len(name)
		if len(name)%2 == 0 {
			n++
		}
		rec := make([]byte, n)
		rec[0] = byte(n)
		both(rec[2:10], extent)
		both(rec[10:18], size)
		rec[18], rec[19], rec[20] = 125, 6, 15
		if isDir {
			rec[25] = 0x02
		}
		rec[28], rec[31] = 1, 1
		rec[32] = byte(len(name))
		copy(rec[33:], name)
		return rec
	}

	pvd := sector(lbaPVD)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], bytes.Repeat([]byte{' '}, 32))
	copy(pvd[40:72], append([]byte("TEST"), bytes.Repeat([]byte{' '}, 28)...))
	both(pvd[80:88], 24)
	copy(pvd[156:190], record("\x00", lbaRoot, iso9660.BlockSize, true))

	term := sector(17)
	term[0] = 255
	copy(term[1:6], "CD001")

	root := sector(lbaRoot)
	off := 0
	for _, rec := range [][]byte{
		record("\x00", lbaRoot, iso9660.BlockSize, true),
		record("\x01", lbaRoot, iso9660.BlockSize, true),
		record("A.TXT;1", lbaData, uint32(len(body)), false),
		record("B.TXT;1", lbaData, uint32(len(body)), false),
	} {
		copy(root[off:], rec)
		off += len(rec)
	}
	copy(sector(lbaData), body)

	fsys, err := iso9660.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("building the test volume: %v", err)
	}
	return fsys
}

// emptyStore is a store with nothing in it, which is what a share whose
// folder has yet to be created looks like.
type emptyStore struct{}

func (emptyStore) Create(string) (io.WriteCloser, error) { return nil, errNoSuchImage }

func (emptyStore) Open(string) (io.ReadSeekCloser, storedFile, error) {
	return nil, storedFile{}, errNoSuchImage
}

func (emptyStore) List() ([]storedFile, error) { return nil, nil }

func (emptyStore) Remove(string) error { return errNoSuchImage }

func (emptyStore) Describe() string { return "//nowhere/share" }

func (emptyStore) Kind() string { return "smb" }

func (emptyStore) Space() (int64, int64, bool) { return 0, 0, false }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the drive to change hands")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
