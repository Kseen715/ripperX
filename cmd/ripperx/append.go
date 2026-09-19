package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

// A disc that has been written but not closed will take another session.
// That is not the same thing as burning: nothing is erased, the files
// already on the disc stay where they are and stay visible, and what is
// written goes in the space that is left.
//
// It is worth having because the alternative, for a disc with a third of a
// gigabyte going spare, is to throw the disc away and burn a new one.
//
// Only xorriso can do this properly. cdrecord will write another session,
// but it writes whatever image it is given - so unless that image was built
// against the previous session, the old files vanish from view even though
// they are still on the disc. xorriso reads the existing filesystem, adds
// to it, and writes a session that refers back to the old data, which is
// what anyone means by "append".

// appendOverhead is the room a new session needs beyond the files
// themselves: its volume descriptors, its directory records and the run-in
// and run-out the drive puts around it. Two megabytes is generous for a
// session of a few files and cheap insurance against a write that runs out
// of disc at the very end.
const appendOverhead = 2 << 20

type appendRequest struct {
	Drive string `json:"drive" doc:"which drive, by its id"`
	// Names are files in the image store to put on the disc.
	Names []string `json:"names" doc:"the files in the image store to add"`
	// Folder is where on the disc they go. Empty puts them in the root.
	Folder string `json:"folder,omitempty" doc:"the directory on the disc to put them in; the root by default"`
	// Verify reads every appended file back off the disc and compares it
	// with what was sent. On by default.
	Verify *bool `json:"verify,omitempty" doc:"read the files back off the disc and check them; defaults to true"`
	// Close finalises the disc so nothing more can ever be added. It is off
	// by default: a disc left open can be appended to again, and closing is
	// the one part of this that cannot be undone.
	Close bool `json:"close,omitempty" doc:"finalise the disc afterwards so no further session can be added"`
}

// appendBlocker says why files cannot be added to this disc, or returns
// empty when they can. It is the counterpart of burnBlocker, and the two
// are deliberately separate: a disc that cannot be burned because it
// already has data on it is very often one that can be appended to, and
// answering "no" to both was wrong.
func (s *server) appendBlocker(caps *mmc.Capabilities, disc *mmc.Disc) string {
	if !s.allowBurn {
		return "this server was started with burning turned off"
	}
	if s.burner == nil {
		return "no burner program is installed"
	}
	if s.burner.kind != "xorriso" {
		return "adding to a disc needs xorriso; " + s.burner.kind +
			" can only write a whole disc at once"
	}
	if caps == nil {
		return "the drive has not said what it can do"
	}
	if !caps.Write.CDR && !caps.Write.CDRW && !caps.Write.DVDR && !caps.Write.DVDPlusR &&
		!caps.Write.DVDRW && !caps.Write.DVDPlusRW && !caps.Write.DVDRAM {
		return "this drive can only read"
	}
	if disc == nil || !disc.Present {
		return "there is no disc in the drive"
	}
	switch disc.Status {
	case mmc.DiscEmpty:
		return "this disc is blank - burn it rather than adding to it"
	case mmc.DiscComplete:
		return "this disc has been closed, so no further session can be written to it"
	}
	if !disc.Appendable || disc.WritableSectors <= 0 {
		return "this disc is full"
	}
	return ""
}

func (s *server) handleAppend(w http.ResponseWriter, r *http.Request) {
	var req appendRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	d, err := s.drives.get(req.Drive)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	caps, disc, err := d.state(2 * time.Second)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if why := s.appendBlocker(caps, disc); why != "" {
		writeJSON(w, http.StatusConflict, errorResponse{Error: why})
		return
	}
	if len(req.Names) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "no files were given to add"})
		return
	}

	// Everything that can be checked before the laser is switched on.
	var files []storedFile
	var total int64
	for _, name := range req.Names {
		if !validName(name) {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: errBadName.Error()})
			return
		}
		f, info, err := s.store.Open(name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
			return
		}
		f.Close()
		files = append(files, info)
		total += info.Size
	}
	folder, err := discFolder(req.Folder)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if room := disc.WritableBytes; total+appendOverhead > room {
		writeJSON(w, http.StatusConflict, errorResponse{Error: fmt.Sprintf(
			"that needs %s and this disc has %s left",
			humanBytes(total+appendOverhead), humanBytes(room))})
		return
	}

	verify := true
	if req.Verify != nil {
		verify = *req.Verify
	}
	label := fmt.Sprintf("%s to the disc in %s",
		plural(int64(len(files)), "file", "files"), d.id)
	// Writing then reading back is two passes over the same bytes.
	jobTotal := total
	if verify {
		jobTotal *= 2
	}

	job, err := s.jobs.startOnDrive(d, "append", label, jobTotal,
		func(ctx context.Context, rec *jobRecord) error {
			return s.appendFiles(ctx, rec, d, files, folder, verify, req.Close)
		})
	if err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

// discFolder cleans the directory a request asks for. The result is always
// absolute and never climbs: it is handed to a burner program as a path
// inside the disc's own filesystem, and a name from a request has no
// business steering that anywhere unexpected.
func discFolder(name string) (string, error) {
	if name == "" {
		return "/", nil
	}
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	for _, part := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if part == "" {
			continue
		}
		if safeName(part, "") != part {
			return "", fmt.Errorf("%q is not a usable directory name on a disc", part)
		}
	}
	return clean, nil
}

func (s *server) appendFiles(ctx context.Context, rec *jobRecord, d *drive, files []storedFile, folder string, verify, closeDisc bool) error {
	// The burner is a separate process and reads from a path, so anything
	// on a share is copied locally first - which is also where a share that
	// has gone away is discovered, before the disc is touched rather than
	// half-way through writing it.
	rec.setPhase("preparing")
	staged := make([]string, 0, len(files))
	sums := make([]string, 0, len(files))
	for _, f := range files {
		p, cleanup, err := s.stageImage(ctx, rec, f)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return err
		}
		sum, err := hashFile(ctx, p)
		if err != nil {
			return err
		}
		staged = append(staged, p)
		sums = append(sums, sum)
	}

	// The burner opens the device itself, and two processes holding it
	// while one reprograms its write parameters is asking for trouble.
	d.mu.Lock()
	if d.dev != nil {
		_ = d.dev.Close()
		d.dev = nil
	}
	d.mu.Unlock()

	rec.setPhase("writing a new session")
	if err := s.runAppend(ctx, rec, d.path, files, staged, folder, closeDisc); err != nil {
		return err
	}

	// The disc is a different disc now: another session, more sectors, a
	// filesystem with more in it.
	d.mu.Lock()
	d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
	d.mu.Unlock()

	var written int64
	for _, f := range files {
		written += f.Size
	}
	rec.progress(written)

	if !verify {
		rec.say("written, not verified")
		return nil
	}
	rec.setPhase("checking what was written")
	return s.verifyAppend(ctx, rec, d, files, sums, folder, written)
}

// xorrisoPercent matches the progress xorriso prints in its own voice,
// which is not the line cdrecord prints and so needs its own pattern:
//
//	xorriso : UPDATE : Writing:   16s   1.0%   fifo  72%  buf 100%   0.0xD
//
// The percentage wanted is the one right after the elapsed time. The two
// that follow it are how full the buffers are, which is why this anchors on
// "Writing:" rather than looking for any number with a per-cent sign after
// it - matching "fifo 72%" would make the bar jump about at random.
var xorrisoPercent = regexp.MustCompile(`Writing:\s+\S+\s+([0-9]+\.[0-9]+)%`)

func (s *server) runAppend(ctx context.Context, rec *jobRecord, dev string, files []storedFile, staged []string, folder string, closeDisc bool) error {
	// xorriso's own interface rather than its cdrecord emulation: the
	// emulation writes whole images, and what is wanted here is to load the
	// filesystem that is already on the disc and add to it.
	args := []string{
		"-abort_on", "FATAL",
		// Loading the existing image is what makes the old files stay
		// visible in the new session.
		"-dev", dev,
	}
	for i, f := range files {
		args = append(args, "-map", staged[i], path.Join(folder, f.Name))
	}
	if closeDisc {
		args = append(args, "-close", "on")
	}
	args = append(args, "-commit", "-eject", "off")

	cmd := exec.CommandContext(ctx, s.burner.path, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", s.burner.path, err)
	}

	var total int64
	for _, f := range files {
		total += f.Size
	}
	var tail []string
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	sc.Split(scanLinesOrCR)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if m := xorrisoPercent.FindStringSubmatch(line); m != nil {
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
				rec.progress(int64(pct / 100 * float64(total)))
			}
			continue
		}
		if m := progressLine.FindStringSubmatch(line); m != nil {
			if done, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				rec.progress(min(done<<20, total))
			}
			continue
		}
		tail = append(tail, line)
		if len(tail) > 12 {
			tail = tail[1:]
		}
		rec.say("%s", line)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("xorriso could not add to this disc: %w\n%s", err, strings.Join(tail, "\n"))
	}
	return nil
}

// verifyAppend reads each file back off the disc through ripperX's own
// filesystem reader and hashes it. A burn is checked by comparing the whole
// disc with one image; there is no such image here, so the check is per
// file - which is the same guarantee, and additionally proves the files are
// where they were asked to go and that the disc's directory can be read at
// all.
func (s *server) verifyAppend(ctx context.Context, rec *jobRecord, d *drive, files []storedFile, sums []string, folder string, base int64) error {
	if err := s.settle(ctx, rec, d); err != nil {
		return err
	}
	// The drive is held by this job, so the filesystem is opened from the
	// device directly rather than through drive.filesystem - that one is
	// the polite path for a request that does not own the drive, and while
	// a job does own it, it answers from the cache this job has just
	// deliberately thrown away. Going through it reported "there is no disc
	// in this drive" about a disc that had just been written.
	dev, err := d.open()
	if err != nil {
		return err
	}
	disc, err := dev.ReadDisc()
	if err != nil {
		return fmt.Errorf("the new session was written but the disc cannot be read back: %w", err)
	}
	fsys, err := iso9660.OpenSession(dev.DataReaderAt(disc.Sectors), disc.LastSessionStart)
	if err != nil {
		return fmt.Errorf("the new session was written but its filesystem cannot be read: %w", err)
	}

	done := base
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := path.Join(folder, f.Name)
		rec.setPhase("checking " + want)

		src, entry, err := fsys.Open(want)
		if err != nil {
			return fmt.Errorf("%s is not on the disc after writing it: %w", want, err)
		}
		if entry.Size != f.Size {
			return fmt.Errorf("%s is %d bytes on the disc and %d bytes in the store",
				want, entry.Size, f.Size)
		}
		h := sha256.New()
		buf := make([]byte, mmc.ChunkData*mmc.SectorData)
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
		if got := hex.EncodeToString(h.Sum(nil)); got != sums[i] {
			return fmt.Errorf("%s reads back as %s but was written from %s: the session did not come out",
				want, got[:16], sums[i][:16])
		}
		rec.addTarget(f.Name)
	}
	rec.say("verified: %s read back off the disc and matched",
		plural(int64(len(files)), "file", "files"))
	return nil
}
