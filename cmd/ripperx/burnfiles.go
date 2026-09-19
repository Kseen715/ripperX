package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

// Burning an archive as the files inside it.
//
// This is the other half of ripping a disc's files. A rip produces one
// archive, which is the right thing to keep and the wrong thing to burn: a
// disc with a single .zip on it is not the disc that was ripped. So the
// archive is unpacked and what was inside is written as a filesystem, and
// the disc that comes out is the disc that went in.
//
// It is a different operation from burning an .iso, which is a copy of a
// disc sector for sector. Here there is no image: xorriso is asked to build
// a filesystem, and what proves it worked is reading every file back off
// the disc and comparing it with what went on.

// unpackOverhead is what the filesystem itself costs on top of the files:
// the descriptors, the directory records and the padding. Two megabytes is
// generous for anything with fewer than a few thousand files in it, and
// being generous here costs nothing next to finding out at the end of a
// burn.
const unpackOverhead = 4 << 20

// burnFiles is the job: fetch the archive, unpack it, write it, read it
// back.
func (s *server) burnFiles(ctx context.Context, rec *jobRecord, d *drive,
	src store, info storedFile, disc *mmc.Disc, req burnRequest, verify bool) error {
	if s.burner == nil {
		return errNoBurner
	}

	rec.setPhase("fetching the archive")
	local, cleanup, err := s.stageImage(ctx, rec, src, info)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "ripperx-unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	rec.setPhase("unpacking " + info.Name)
	files, total, err := unpackArchive(ctx, rec, info.Name, local, dir)
	if err != nil {
		return err
	}
	rec.say("%s, %s", plural(int64(len(files)), "file", "files"), humanBytes(total))

	// Only now is it known how much there is. The check has to be here
	// rather than in the preflight, but it is still before the laser.
	if err := checkFilesFit(total, disc); err != nil {
		return err
	}

	// The bar measured the unpacking until now; from here it measures the
	// write, and the read back when there is one.
	written := total
	if verify {
		written *= 2
	}
	rec.setTotal(written)
	rec.progress(0)

	// The burner opens the device itself, and two processes holding it while
	// one reprograms its write parameters is asking for trouble.
	d.mu.Lock()
	if d.dev != nil {
		_ = d.dev.Close()
		d.dev = nil
	}
	d.mu.Unlock()

	rec.setPhase("writing the disc")
	err = s.author(ctx, rec, authorJob{
		dev:       d.path,
		maps:      [][2]string{{dir, "/"}},
		total:     total,
		volume:    discVolumeName(info.Name),
		speedX:    req.SpeedX,
		closeDisc: true,
		failed:    "xorriso could not write these files to the disc",
	})

	// Whatever happened, the disc is not what it was.
	d.mu.Lock()
	d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
	d.mu.Unlock()
	if err != nil {
		return err
	}
	rec.progress(total)

	if !verify {
		rec.say("written, not verified")
		return nil
	}
	rec.setPhase("checking what was written")
	return s.verifyFiles(ctx, rec, d, files, total)
}

// discVolumeName is what the disc will call itself. An ISO 9660 volume
// identifier is a short, upper-case thing, and a disc whose label is the
// name of the archive it came from is easier to find later than one called
// CDROM.
func discVolumeName(archive string) string {
	name := archive
	for {
		idx, ok := archiveOf(name)
		if !ok {
			break
		}
		name = name[:len(name)-len(archiveExtensions[idx].suffix)]
	}
	// The timestamp a rip appends is noise on a label with 32 characters in
	// it; the name is what identifies the disc.
	if cut := strings.LastIndex(name, "-20"); cut > 0 && len(name)-cut >= 9 {
		name = name[:cut]
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(safeName(name, "DISC")) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "DISC"
	}
	return out
}

// checkFilesFit is the check that saves discs, for a write whose size is
// only known once the archive has been opened.
func checkFilesFit(total int64, disc *mmc.Disc) error {
	capacity := disc.WritableBytes
	if capacity == 0 {
		sectors := disc.BlankSectors
		if sectors == 0 {
			sectors = disc.Sectors
		}
		capacity = sectors * mmc.SectorData
	}
	if capacity > 0 && total+unpackOverhead > capacity {
		return fmt.Errorf("these files need %s and this disc holds %s",
			humanBytes(total+unpackOverhead), humanBytes(capacity))
	}
	return nil
}

// verifyFiles reads every file back off the disc and hashes it. Burning an
// image is checked by comparing the whole disc with the image; there is no
// image here, so the check is per file - which is the same guarantee, and
// additionally proves the directory came out and the files are where they
// were meant to go.
func (s *server) verifyFiles(ctx context.Context, rec *jobRecord, d *drive,
	files []unpackedFile, base int64) error {
	if err := s.settle(ctx, rec, d); err != nil {
		return err
	}
	// The drive is held by this job, so the filesystem is opened from the
	// device directly rather than through drive.filesystem: that one answers
	// from a cache this job has just deliberately thrown away.
	dev, err := d.open()
	if err != nil {
		return err
	}
	disc, err := dev.ReadDisc()
	if err != nil {
		return fmt.Errorf("the disc was written but cannot be read back: %w", err)
	}
	fsys, err := iso9660.OpenSession(dev.DataReaderAt(disc.Sectors), disc.LastSessionStart)
	if err != nil {
		return fmt.Errorf("the disc was written but its filesystem cannot be read: %w", err)
	}

	done := base
	buf := make([]byte, mmc.ChunkData*mmc.SectorData)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := path.Join("/", f.rel)
		rec.setPhase("checking " + want)

		src, entry, err := fsys.Open(want)
		if err != nil {
			return fmt.Errorf("%s is not on the disc after writing it: %w", want, err)
		}
		if entry.Size != f.size {
			return fmt.Errorf("%s is %d bytes on the disc and was %d bytes in the archive",
				want, entry.Size, f.size)
		}
		h := sha256.New()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := src.Read(buf)
			if n > 0 {
				h.Write(buf[:n])
				done += int64(n)
				rec.progress(done)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return fmt.Errorf("reading %s back off the disc: %w", want, rerr)
			}
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != f.sum {
			return fmt.Errorf("%s reads back as %s but was written from %s: "+
				"the disc did not come out", want, got[:16], f.sum[:16])
		}
	}
	rec.setPhase("")
	rec.say("every one of the %s reads back exactly as it went on",
		plural(int64(len(files)), "file", "files"))
	return nil
}
