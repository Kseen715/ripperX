package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/mmc"
)

// Burning is the one thing ripperX does not do itself. Writing a disc means
// getting MODE SELECT, the write parameters page, the track descriptors and
// the close sequence right for a particular drive's firmware, and getting
// any of it wrong costs a disc that cannot be un-ruined. xorriso and
// cdrecord have been getting it right across two decades of drives; this
// hands the write to whichever is installed.
//
// What ripperX keeps is everything either side of the write: refusing a
// burn that cannot work before a disc is spoiled, and proving afterwards
// that what is on the disc is what was meant to be, by reading every sector
// back and comparing it with the source.

type burner struct {
	path string
	kind string // xorriso, cdrecord or wodim
}

// burnerCandidates are tried in order. xorriso first: it is maintained, it
// is in every distribution, and its cdrecord emulation takes the same
// arguments as the other two, so one command line serves all three.
var burnerCandidates = []struct{ bin, kind string }{
	{"xorriso", "xorriso"},
	{"cdrecord", "cdrecord"},
	{"wodim", "wodim"},
}

func findBurner(explicit string) *burner {
	if explicit != "" {
		kind := strings.ToLower(filepath.Base(explicit))
		for _, c := range burnerCandidates {
			if strings.Contains(kind, c.bin) {
				kind = c.kind
				break
			}
		}
		if _, err := exec.LookPath(explicit); err != nil {
			return nil
		}
		return &burner{path: explicit, kind: kind}
	}
	for _, c := range burnerCandidates {
		if p, err := exec.LookPath(c.bin); err == nil {
			return &burner{path: p, kind: c.kind}
		}
	}
	return nil
}

// args builds the command line. All three programs accept cdrecord's, which
// is why xorriso is invoked through its emulation rather than its own
// interface: one set of arguments, one progress format to parse.
func (b *burner) args(dev, image string, speedX int, dummy, eject bool) []string {
	var a []string
	if b.kind == "xorriso" {
		a = append(a, "-as", "cdrecord")
	}
	a = append(a, "-v", "dev="+dev)
	if speedX > 0 {
		a = append(a, "speed="+strconv.Itoa(speedX))
	}
	// Session at once: the whole disc written in one pass, which is what
	// makes the result byte-for-byte the image rather than the image plus
	// the run-out blocks track-at-once leaves between tracks.
	a = append(a, "-sao")
	if dummy {
		a = append(a, "-dummy")
	}
	if eject {
		a = append(a, "-eject")
	}
	return append(a, "-data", image)
}

func (b *burner) blankArgs(dev string, full bool) []string {
	var a []string
	if b.kind == "xorriso" {
		a = append(a, "-as", "cdrecord")
	}
	mode := "fast"
	if full {
		mode = "all"
	}
	return append(a, "-v", "dev="+dev, "blank="+mode)
}

type burnRequest struct {
	Drive string `json:"drive" doc:"which drive, by its id"`
	Image string `json:"image" doc:"the image in the store to write"`
	// SpeedX is a multiple of the disc's 1x, which is what burner programs
	// take. 0 lets the drive choose, which is usually its fastest.
	SpeedX int `json:"speedX,omitempty" doc:"write speed as a multiple of 1x; 0 lets the drive decide"`
	// Dummy runs the whole burn with the write laser off. It proves the
	// drive keeps up with the source at this speed without spending a disc.
	Dummy bool `json:"dummy,omitempty" doc:"rehearse the burn with the laser off, writing nothing"`
	// Verify reads every sector back and compares it with the image. On by
	// default, because a burn nobody checked is a burn nobody can trust.
	Verify *bool `json:"verify,omitempty" doc:"read the disc back and compare it with the image; defaults to true"`
	Eject  bool  `json:"eject,omitempty" doc:"open the tray when everything is finished"`
}

func (s *server) handleBurn(w http.ResponseWriter, r *http.Request) {
	var req burnRequest
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
	if why := s.burnBlocker(caps, disc); why != "" {
		writeJSON(w, http.StatusConflict, errorResponse{Error: why})
		return
	}
	if !validName(req.Image) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: errBadName.Error()})
		return
	}
	f, info, err := s.store.Open(req.Image)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	f.Close()

	if err := checkImageFits(info, disc); err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}

	verify := true
	if req.Verify != nil {
		verify = *req.Verify
	}
	if req.Dummy {
		// There is nothing on the disc afterwards to compare against.
		verify = false
	}

	label := fmt.Sprintf("%s to %s", req.Image, d.id)
	if req.Dummy {
		label = "rehearsal of " + label
	}
	// The job's total counts the write and, when asked for, the read back:
	// one bar that fills once rather than two that each fill to half.
	total := info.Size
	if verify {
		total *= 2
	}
	job, err := s.jobs.startOnDrive(d, "burn", label, total,
		func(ctx context.Context, rec *jobRecord) error {
			return s.burn(ctx, rec, d, info, req, verify)
		})
	if err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

// checkImageFits is the check that saves discs. An image whose length is
// not a whole number of 2048-byte sectors is not a disc image at all, and
// one larger than the disc runs out of room several minutes into a burn.
func checkImageFits(info storedFile, disc *mmc.Disc) error {
	if info.Size == 0 {
		return fmt.Errorf("%s is empty", info.Name)
	}
	if info.Size%mmc.SectorData != 0 {
		hint := ""
		if info.Size%mmc.SectorRaw == 0 {
			hint = "; it looks like a raw 2352-byte image - convert it to an .iso first"
		}
		return fmt.Errorf("%s is %d bytes, which is not a whole number of 2048-byte sectors%s",
			info.Name, info.Size, hint)
	}
	capacity := disc.BlankSectors
	if capacity == 0 {
		capacity = disc.Sectors
	}
	if capacity > 0 && info.Size > capacity*mmc.SectorData {
		return fmt.Errorf("%s needs %s and this disc holds %s",
			info.Name, humanBytes(info.Size), humanBytes(capacity*mmc.SectorData))
	}
	return nil
}

func (s *server) burn(ctx context.Context, rec *jobRecord, d *drive, info storedFile, req burnRequest, verify bool) error {
	if s.burner == nil {
		return errors.New("no burner program is installed")
	}

	// The burner is a separate process and needs a path, so an image on a
	// share is staged to a local file first. A burn reading over the
	// network at the speed the laser demands is how a buffer underrun
	// happens, so this is the right thing to do even where it would work.
	rec.setPhase("preparing")
	imagePath, cleanup, err := s.stageImage(ctx, rec, info)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}

	// The source hash is taken before the write, from the same file the
	// burner will read, so the verification afterwards compares the disc
	// with what was actually sent to it.
	rec.setPhase("hashing the image")
	sourceSum, err := hashFile(ctx, imagePath)
	if err != nil {
		return err
	}
	rec.setHash(sourceSum)

	// The burner opens the device itself, and two processes holding it
	// while one reprograms its write parameters is asking for trouble.
	d.mu.Lock()
	if d.dev != nil {
		_ = d.dev.Close()
		d.dev = nil
	}
	d.mu.Unlock()

	rec.setPhase("writing")
	if req.Dummy {
		rec.setPhase("rehearsing")
	}
	if err := s.runBurner(ctx, rec, d.path, imagePath, info.Size, req); err != nil {
		return err
	}
	// Forget everything known about the disc: it is a different disc now.
	d.mu.Lock()
	d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
	d.mu.Unlock()

	if !verify {
		if req.Dummy {
			rec.say("the rehearsal finished; the disc is untouched")
		} else {
			rec.say("written, not verified")
		}
		return nil
	}

	rec.setPhase("verifying")
	return s.verifyBurn(ctx, rec, d, imagePath, info.Size, sourceSum)
}

// stageImage returns a local path for the image. A local store already has
// one; anything else is copied to a temporary file, which is also the point
// at which a share that has gone away is discovered - before the disc is
// spoiled rather than in the middle of writing it.
func (s *server) stageImage(ctx context.Context, rec *jobRecord, info storedFile) (string, func(), error) {
	if local, ok := s.store.(*localStore); ok {
		if p, ok := local.localPath(info.Name); ok {
			return p, nil, nil
		}
	}
	src, _, err := s.store.Open(info.Name)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "ripperx-*.iso")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	rec.say("copying %s from %s before writing", info.Name, s.store.Describe())
	if _, err := copyCtx(ctx, tmp, src, info.Size); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmp.Name(), cleanup, nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// progressLine matches the one line all three burners print as they write:
// "Track 01:   12 of  250 MB written (fifo 100%) [buf  99%]  4.0x."
var progressLine = regexp.MustCompile(`(\d+) of\s+(\d+) MB written`)

func (s *server) runBurner(ctx context.Context, rec *jobRecord, dev, image string, size int64, req burnRequest) error {
	args := s.burner.args(dev, image, req.SpeedX, req.Dummy, false)
	cmd := exec.CommandContext(ctx, s.burner.path, args...)
	// The burner talks on stderr and says nothing useful on stdout.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", s.burner.path, err)
	}

	// The last lines are kept so a failure can quote the burner's own words
	// rather than only its exit status, which on cdrecord is always 1.
	var tail []string
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	// Both a newline and a carriage return end a line here: the progress
	// counter rewrites one line with \r and would otherwise never be seen.
	sc.Split(scanLinesOrCR)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if m := progressLine.FindStringSubmatch(line); m != nil {
			if done, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				rec.progress(min(done<<20, size))
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
		return fmt.Errorf("%s failed: %w\n%s", filepath.Base(s.burner.path), err, strings.Join(tail, "\n"))
	}
	rec.progress(size)
	return nil
}

// scanLinesOrCR splits on either line ending, so a progress counter that
// rewrites its line with a carriage return is read as it happens instead of
// arriving as one enormous line when the burn finishes.
func scanLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// verifyBurn reads the disc back and compares it with the image, sector by
// sector. Both a hash and a first-difference address come out of the one
// pass: the hash is what to record, and the address is what tells you
// whether the disc failed at the very end - a burn that ran out of disc -
// or in the middle, which is a different fault entirely.
func (s *server) verifyBurn(ctx context.Context, rec *jobRecord, d *drive, imagePath string, size int64, sourceSum string) error {
	if err := s.settle(ctx, rec, d); err != nil {
		return err
	}
	dev, err := d.open()
	if err != nil {
		return err
	}

	src, err := os.Open(imagePath)
	if err != nil {
		return err
	}
	defer src.Close()

	sectors := size / mmc.SectorData
	h := sha256.New()
	disc := make([]byte, mmc.ChunkData*mmc.SectorData)
	want := make([]byte, mmc.ChunkData*mmc.SectorData)
	base := rec.snapshot().Done

	for lba := int64(0); lba < sectors; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := mmc.ChunkData
		if rest := sectors - lba; rest < int64(count) {
			count = int(rest)
		}
		n := count * mmc.SectorData
		if _, err := io.ReadFull(src, want[:n]); err != nil {
			return fmt.Errorf("reading the image back at sector %d: %w", lba, err)
		}
		if err := dev.ReadData(lba, count, disc); err != nil {
			return fmt.Errorf("the disc could not be read back at sector %d, so the burn did not work: %w", lba, err)
		}
		h.Write(disc[:n])
		if !bytes.Equal(disc[:n], want[:n]) {
			return fmt.Errorf("the disc differs from %s at sector %d: the burn did not come out",
				filepath.Base(imagePath), lba+int64(firstDiff(disc[:n], want[:n]))/mmc.SectorData)
		}
		lba += int64(count)
		rec.progress(base + lba*mmc.SectorData)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if sum != sourceSum {
		// Every sector matched but the whole does not: that can only be a
		// bug here, and saying so is better than reporting success.
		return fmt.Errorf("every sector matched but the disc hashes to %s and the image to %s", sum, sourceSum)
	}
	rec.say("verified: %d sectors read back, SHA-256 %s", sectors, sum)
	return nil
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return 0
}

// settle makes the drive read the disc it has just written. A drive holds
// the table of contents it had when the disc went in, so without this the
// verification reads the old disc's layout - or nothing at all. Spinning
// the disc down and up is enough on most drives; the tray is only cycled
// when it is not.
func (s *server) settle(ctx context.Context, rec *jobRecord, d *drive) error {
	rec.say("waiting for the drive to re-read the disc")
	d.mu.Lock()
	if d.dev != nil {
		_ = d.dev.Close()
		d.dev = nil
	}
	d.mu.Unlock()

	dev, err := d.open()
	if err != nil {
		return err
	}
	if err := dev.WaitReady(60 * time.Second); err == nil {
		if disc, err := dev.ReadDisc(); err == nil && disc.Present && disc.Sectors > 0 {
			return nil
		}
	}

	// Cycle the tray: the drive is made to load the disc as if it were new.
	rec.say("the drive is still holding the old table of contents; cycling the tray")
	_ = dev.Eject()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	if err := dev.LoadTray(); err != nil {
		return fmt.Errorf("the tray could not be closed again, so the disc cannot be checked: %w", err)
	}
	if err := dev.WaitReady(60 * time.Second); err != nil {
		return fmt.Errorf("the drive did not become ready after the burn: %w", err)
	}
	return nil
}

type eraseRequest struct {
	Drive string `json:"drive" doc:"which drive, by its id"`
	Full  bool   `json:"full,omitempty" doc:"erase every sector rather than only the table of contents; slow, and what a disc that will not take a burn needs"`
}

// handleErase blanks a rewritable disc. A fast erase clears the table of
// contents in under a minute and is enough to write the disc again; a full
// erase rewrites every sector and takes as long as a burn, and is what
// rescues a disc that a fast erase has left unwritable.
func (s *server) handleErase(w http.ResponseWriter, r *http.Request) {
	var req eraseRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	d, err := s.drives.get(req.Drive)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	if !s.allowBurn || s.burner == nil {
		writeJSON(w, http.StatusForbidden,
			errorResponse{Error: "this server cannot write to discs"})
		return
	}
	_, disc, err := d.state(2 * time.Second)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if disc == nil || !disc.Present {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "there is no disc in this drive"})
		return
	}
	if !disc.Erasable {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: fmt.Sprintf("a %s cannot be erased; only a rewritable disc can", disc.ProfileName)})
		return
	}

	kind := "fast"
	if req.Full {
		kind = "full"
	}
	job, err := s.jobs.startOnDrive(d, "erase",
		fmt.Sprintf("%s erase of the disc in %s", kind, d.id), 0,
		func(ctx context.Context, rec *jobRecord) error {
			d.mu.Lock()
			if d.dev != nil {
				_ = d.dev.Close()
				d.dev = nil
			}
			d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
			d.mu.Unlock()

			rec.setPhase("erasing")
			cmd := exec.CommandContext(ctx, s.burner.path, s.burner.blankArgs(d.path, req.Full)...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("%s failed: %w\n%s", filepath.Base(s.burner.path), err, lastLines(out, 8))
			}
			rec.say("the disc is blank again")
			return nil
		})
	if err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
