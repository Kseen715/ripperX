package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/discfs"
	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

// Ripping is the same shape whatever is being ripped: claim the drive, work
// out how many bytes are coming, read them in chunks, write them to the
// store while hashing them, and report where the drive gave up. What
// differs between an .iso, an .img, an audio track and a tar of files is
// only where the bytes come from.

type ripRequest struct {
	Drive string `json:"drive" doc:"which drive, by its id"`
	Kind  string `json:"kind" doc:"iso, img, audio or files"`
	Name  string `json:"name,omitempty" doc:"base name for the file written; taken from the disc's label when empty"`
	// Paths is for kind=files: what to pull off the disc. A directory is
	// taken with everything under it.
	Paths []string `json:"paths,omitempty" doc:"for kind=files, the paths on the disc to rip"`
	// Tracks is for kind=audio; empty means every audio track.
	Tracks []int `json:"tracks,omitempty" doc:"for kind=audio, which track numbers; empty means all of them"`
	// SpeedKB caps the read speed for this rip only. Slowing a drive down
	// is what rescues a scratched disc.
	SpeedKB int `json:"speedKb,omitempty" doc:"read speed cap in kilobytes a second; 0 uses the server's setting"`
	// Length picks how much of the disc an image covers: the whole data
	// track as the drive reports it, or only the sectors the ISO 9660
	// volume claims. They differ on a disc with padding at the end, and the
	// volume figure is the one that matches an image made by the disc's
	// author.
	Length string `json:"length,omitempty" doc:"track (default, the whole data track) or volume (only what the filesystem claims)"`
	// Format is for kind=files when more than one file is taken: which
	// archive they are wrapped in. One of the ids /api/formats lists.
	Format string `json:"format,omitempty" doc:"for kind=files, the archive format; zip by default"`
}

type ripResponse struct {
	Job Job `json:"job" doc:"the job that was started"`
}

func (s *server) handleRip(w http.ResponseWriter, r *http.Request) {
	var req ripRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "malformed request"})
		return
	}
	d, err := s.drives.get(req.Drive)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
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

	job, err := s.startRip(d, disc, req)
	if err != nil {
		if driveUnavailable(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

// ripName is the name a rip is written under, and whether it was chosen
// rather than invented.
//
// A name somebody typed is the name, exactly - that is what asking for one
// means, and "movies" coming back as "movies-20260919-211247.zip" is not a
// rename. An invented name carries the date instead, so that two rips of
// the same disc never collide.
//
// What is invented depends on what was asked for. A rip of one folder is
// called after that folder: it is what the person pressing the button is
// looking at, and it is already what the same folder downloaded to a
// browser is called. Only a rip of the whole disc falls back to the disc.
func (s *server) ripName(d *drive, disc *mmc.Disc, requested, suggested string) (string, bool) {
	if name := safeName(requested, ""); name != "" {
		return name, true
	}
	base := safeName(suggested, "")
	if base == "" {
		if fsys, err := d.filesystem(); err == nil {
			base = safeName(fsys.Volume().VolumeID, "")
		}
	}
	if base == "" {
		base = safeName(disc.ProfileName, "disc")
	}
	return base + "-" + time.Now().Format("20060102-150405"), false
}

// suggestFrom is what a rip of these paths is called when nobody said. One
// path names itself; several have no one name between them, so the disc
// does it. Taking the root is taking the disc, and is named after it.
func suggestFrom(paths []string) string {
	if len(paths) != 1 {
		return ""
	}
	clean := path.Clean("/" + strings.TrimPrefix(paths[0], "/"))
	if clean == "/" {
		return ""
	}
	return path.Base(clean)
}

// unique keeps a rip from replacing one that is already there. Silently
// overwriting a four-gigabyte image because its name was typed twice is not
// a thing to do, so the second one gets a number.
func (s *server) unique(name string) string {
	files, err := s.store.List()
	if err != nil {
		return name
	}
	taken := make(map[string]bool, len(files))
	for _, f := range files {
		taken[strings.ToLower(f.Name)] = true
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	stem, ext := splitExtension(name)
	for i := 2; i < 1000; i++ {
		try := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !taken[strings.ToLower(try)] {
			return try
		}
	}
	return stem + "-" + time.Now().Format("20060102-150405") + ext
}

// splitExtension keeps a two-part archive suffix together, so a second
// "disc.tar.gz" becomes "disc-2.tar.gz" and not "disc.tar-2.gz".
func splitExtension(name string) (stem, ext string) {
	if i, ok := archiveOf(name); ok {
		suffix := archiveExtensions[i].suffix
		return name[:len(name)-len(suffix)], name[len(name)-len(suffix):]
	}
	ext = path.Ext(name)
	return strings.TrimSuffix(name, ext), ext
}

// withExtension keeps the extension of what a file came from when the name
// chosen for it has none, so renaming VTS_01_1.VOB to "opening scene" still
// produces something a player will open.
func withExtension(name, like string) string {
	if path.Ext(name) != "" {
		return name
	}
	return name + path.Ext(like)
}

func (s *server) startRip(d *drive, disc *mmc.Disc, req ripRequest) (*Job, error) {
	speed := req.SpeedKB
	if speed == 0 {
		speed = s.readSpeedKB
	}
	base, chosen := s.ripName(d, disc, req.Name, suggestFrom(req.Paths))

	switch req.Kind {
	case "iso":
		if disc.DataTracks == 0 {
			return nil, errors.New("this disc has no data track, so there is nothing to put in an .iso")
		}
		sectors := disc.Sectors
		if req.Length == "volume" {
			fsys, err := d.filesystem()
			if err != nil {
				return nil, fmt.Errorf("the volume's own length was asked for, but its descriptors could not be read: %w", err)
			}
			if n := fsys.Volume().Sectors; n > 0 && n <= sectors {
				sectors = n
			}
		}
		name := s.unique(base + ".iso")
		return s.jobs.startOnDrive(d, "iso",
			fmt.Sprintf("%s from %s", name, d.id), sectors*mmc.SectorData,
			func(ctx context.Context, rec *jobRecord) error {
				return s.ripImage(ctx, rec, d, name, 0, sectors, speed, false)
			})

	case "img":
		if !disc.RawReadable {
			return nil, errors.New("this drive will not hand over raw sectors from this disc, so an exact .img is not possible; rip an .iso instead")
		}
		name := s.unique(base + ".img")
		return s.jobs.startOnDrive(d, "img",
			fmt.Sprintf("%s from %s", name, d.id), disc.Sectors*mmc.SectorRaw,
			func(ctx context.Context, rec *jobRecord) error {
				if err := s.ripImage(ctx, rec, d, name, 0, disc.Sectors, speed, true); err != nil {
					return err
				}
				// A raw image without a cue sheet is a file nothing knows
				// how to mount. Writing one beside it costs a few hundred
				// bytes and makes the image usable.
				return s.writeCue(rec, base, name, disc)
			})

	case "audio":
		tracks := selectAudioTracks(disc, req.Tracks)
		if len(tracks) == 0 {
			return nil, errors.New("this disc has no audio tracks to rip")
		}
		if !disc.RawReadable {
			return nil, errors.New("this drive will not read audio sectors from this disc")
		}
		var total int64
		for _, t := range tracks {
			total += t.Sectors * mmc.SectorRaw
		}
		return s.jobs.startOnDrive(d, "audio",
			fmt.Sprintf("%d audio track(s) from %s", len(tracks), d.id), total,
			func(ctx context.Context, rec *jobRecord) error {
				return s.ripAudio(ctx, rec, d, base, tracks, speed)
			})

	case "files":
		if len(req.Paths) == 0 {
			return nil, errors.New("no paths were given to rip")
		}
		fsys, err := d.filesystem()
		if err != nil {
			return nil, err
		}
		plan, err := planFiles(fsys, req.Paths)
		if err != nil {
			return nil, err
		}
		if len(plan.files) == 0 {
			return nil, errors.New("nothing to rip: the paths given hold no files")
		}
		format, ok := archiveByID(req.Format)
		if !ok {
			return nil, fmt.Errorf("no such archive format %q", req.Format)
		}
		return s.jobs.startOnDrive(d, "files", plan.label(d.id), plan.bytes,
			func(ctx context.Context, rec *jobRecord) error {
				return s.ripFiles(ctx, rec, d, base, chosen, fsys, plan, format, speed)
			})
	}
	return nil, fmt.Errorf("no such rip kind %q; it is one of iso, img, audio or files", req.Kind)
}

func selectAudioTracks(disc *mmc.Disc, want []int) []mmc.Track {
	var out []mmc.Track
	for _, t := range disc.Tracks {
		if !t.Audio || t.Sectors <= 0 {
			continue
		}
		if len(want) == 0 {
			out = append(out, t)
			continue
		}
		for _, n := range want {
			if n == t.Number {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// sink is a file being written in the store, with the hash of what went
// into it and the job's progress counter kept up to date as it goes. The
// hash is computed on the way past rather than by reading the file back:
// on a share that is the difference between one pass and two.
type sink struct {
	w      io.WriteCloser
	hash   hash.Hash
	rec    *jobRecord
	name   string
	offset int64 // bytes already counted towards the job before this file
	n      int64
	closed bool
}

func (s *server) newSink(rec *jobRecord, name string, offset int64) (*sink, error) {
	f, err := s.store.Create(name)
	if err != nil {
		return nil, fmt.Errorf("creating %s in %s: %w", name, s.store.Describe(), err)
	}
	return &sink{w: f, hash: sha256.New(), rec: rec, name: name, offset: offset}, nil
}

func (k *sink) Write(p []byte) (int, error) {
	n, err := k.w.Write(p)
	if n > 0 {
		k.hash.Write(p[:n])
		k.n += int64(n)
		k.rec.progress(k.offset + k.n)
	}
	return n, err
}

// plain is the sink as a writer that still hashes what goes through it but
// does not count it towards the job's progress. An archive is measured in
// the bytes read off the disc rather than the bytes written - a compressed
// archive has no length until it is finished - so the two must not both
// move the same bar.
func (k *sink) plain() io.Writer { return plainSink{k} }

type plainSink struct{ k *sink }

func (p plainSink) Write(b []byte) (int, error) {
	n, err := p.k.w.Write(b)
	if n > 0 {
		p.k.hash.Write(b[:n])
		p.k.n += int64(n)
	}
	return n, err
}

func (k *sink) sum() string { return hex.EncodeToString(k.hash.Sum(nil)) }

// Close is idempotent, because every writer here closes the sink to find
// out whether the file landed and then closes it again from a deferred
// cleanup that cannot know it already happened.
//
// A second close of a local file is harmless. A second close of a file on
// an SMB share unmounts an unmounted share and logs off a logged-off
// session, and that hangs - so a rip to a share wrote its file correctly
// and then never finished, holding the drive and reaching neither the
// history nor the page. Nothing but a share shows it.
func (k *sink) Close() error {
	if k.closed {
		return nil
	}
	k.closed = true
	return k.w.Close()
}

// ripImage reads a run of sectors straight to a file. raw picks between the
// 2048 bytes a filesystem sees and the 2352 bytes actually on the disc.
func (s *server) ripImage(ctx context.Context, rec *jobRecord, d *drive, name string, from, sectors int64, speedKB int, raw bool) error {
	sectorSize, chunk := int64(mmc.SectorData), mmc.ChunkData
	if raw {
		sectorSize, chunk = mmc.SectorRaw, mmc.ChunkRaw
	}
	total := sectors * sectorSize
	if err := s.checkRoom(total); err != nil {
		return err
	}
	rec.setTotal(total)

	dev, err := d.open()
	if err != nil {
		return err
	}
	if speedKB > 0 {
		if err := dev.SetSpeed(speedKB); err != nil {
			rec.say("the drive would not accept a speed limit: %v", err)
		}
	}

	out, err := s.newSink(rec, name, 0)
	if err != nil {
		return err
	}
	// A failed rip leaves no half-file behind for someone to burn later.
	committed := false
	defer func() {
		out.Close()
		if !committed {
			_ = s.store.Remove(name)
		}
	}()

	buf := make([]byte, chunk*int(sectorSize))
	for lba := from; lba < from+sectors; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := chunk
		if rest := from + sectors - lba; rest < int64(count) {
			count = int(rest)
		}
		var bad []int64
		if raw {
			bad, err = dev.ReadRawRetry(lba, count, mmc.SectorAny, buf)
		} else {
			bad, err = dev.ReadDataRetry(lba, count, buf)
		}
		if err != nil {
			return fmt.Errorf("reading sector %d: %w", lba, err)
		}
		rec.markBad(bad)
		if _, err := out.Write(buf[:int64(count)*sectorSize]); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		lba += int64(count)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("finishing %s: %w", name, err)
	}
	committed = true
	rec.setHash(out.sum())
	rec.addTarget(name)
	if n := rec.snapshot().BadSectors; n > 0 {
		rec.say("%d sectors could not be read and are zeroes in the image", n)
	}
	return nil
}

// writeCue puts a cue sheet beside a raw image, so the .img can be mounted,
// burned or opened by anything that understands a disc rather than a file.
func (s *server) writeCue(rec *jobRecord, base, imgName string, disc *mmc.Disc) error {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE \"%s\" BINARY\r\n", imgName)
	for _, t := range disc.Tracks {
		mode := "MODE1/2352"
		if t.Audio {
			mode = "AUDIO"
		}
		fmt.Fprintf(&b, "  TRACK %02d %s\r\n", t.Number, mode)
		// A cue sheet's index is written as minutes:seconds:frames, counted
		// from the start of the file - which is two seconds behind the
		// disc's own addressing, because the lead-in is not in the file.
		fmt.Fprintf(&b, "    INDEX 01 %s\r\n", msf(t.Start))
	}
	name := base + ".cue"
	f, err := s.store.Create(name)
	if err != nil {
		return fmt.Errorf("creating %s: %w", name, err)
	}
	if _, err := io.WriteString(f, b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	rec.addTarget(name)
	return nil
}

// msf formats a sector number the way a cue sheet wants it.
func msf(lba int64) string {
	if lba < 0 {
		lba = 0
	}
	f := lba % mmc.FramesPerSecond
	sec := lba / mmc.FramesPerSecond
	return fmt.Sprintf("%02d:%02d:%02d", sec/60, sec%60, f)
}

// ripAudio writes one .wav per track. WAV rather than FLAC or MP3 because
// the bytes on the disc are already PCM: a header is put in front of them
// and nothing is decoded, re-encoded or lost - and a browser plays the
// result without a plugin.
func (s *server) ripAudio(ctx context.Context, rec *jobRecord, d *drive, base string, tracks []mmc.Track, speedKB int) error {
	var total int64
	for _, t := range tracks {
		total += t.Sectors*mmc.SectorRaw + wavHeaderLen
	}
	if err := s.checkRoom(total); err != nil {
		return err
	}
	rec.setTotal(total)

	dev, err := d.open()
	if err != nil {
		return err
	}
	if speedKB > 0 {
		if err := dev.SetSpeed(speedKB); err != nil {
			rec.say("the drive would not accept a speed limit: %v", err)
		}
	}

	var written int64
	sums := sha256.New()
	for _, t := range tracks {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := fmt.Sprintf("%s-track%02d.wav", base, t.Number)
		rec.setPhase(fmt.Sprintf("track %d", t.Number))
		if err := s.ripOneTrack(ctx, rec, dev, name, t, written, sums); err != nil {
			return err
		}
		written += t.Sectors*mmc.SectorRaw + wavHeaderLen
		rec.addTarget(name)
	}
	// One hash over every track in order: what identifies this rip of this
	// disc, where a per-file hash would identify only a track.
	rec.setHash(hex.EncodeToString(sums.Sum(nil)))
	return nil
}

func (s *server) ripOneTrack(ctx context.Context, rec *jobRecord, dev *mmc.Drive, name string, t mmc.Track, offset int64, overall hash.Hash) error {
	out, err := s.newSink(rec, name, offset)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		out.Close()
		if !committed {
			_ = s.store.Remove(name)
		}
	}()

	if _, err := out.Write(wavHeader(t.Sectors * mmc.SectorRaw)); err != nil {
		return err
	}
	buf := make([]byte, mmc.ChunkRaw*mmc.SectorRaw)
	for lba := t.Start; lba < t.Start+t.Sectors; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := mmc.ChunkRaw
		if rest := t.Start + t.Sectors - lba; rest < int64(count) {
			count = int(rest)
		}
		bad, err := dev.ReadRawRetry(lba, count, mmc.SectorCDDA, buf)
		if err != nil {
			return fmt.Errorf("reading audio sector %d: %w", lba, err)
		}
		rec.markBad(bad)
		chunk := buf[:count*mmc.SectorRaw]
		overall.Write(chunk)
		if _, err := out.Write(chunk); err != nil {
			return err
		}
		lba += int64(count)
	}
	if err := out.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

// filePlan is what a kind=files rip will do, worked out before the job
// starts so the page can be told the size up front and so a mistyped path
// fails immediately rather than half-way through.
type filePlan struct {
	files  []iso9660.Entry
	bytes  int64
	single bool // exactly one file was asked for, so it is written as itself
	// root is the part of every path that is not worth carrying into the
	// archive: the directory the chosen things sit in. Taking /Drivers/Win7
	// gives an archive with Win7 at its root rather than one with a Drivers
	// folder holding a Win7 folder holding the files.
	root string
}

// under returns the name an entry takes inside the archive.
func (p filePlan) under(e iso9660.Entry) string {
	name := strings.TrimPrefix(e.Path, "/")
	if p.root != "" {
		name = strings.TrimPrefix(name, p.root+"/")
	}
	return name
}

func (p filePlan) label(driveID string) string {
	if p.single {
		return fmt.Sprintf("%s from %s", path.Base(p.files[0].Path), driveID)
	}
	return fmt.Sprintf("%d files from %s", len(p.files), driveID)
}

func planFiles(fsys discfs.FS, paths []string) (filePlan, error) {
	var plan filePlan
	seen := map[string]bool{}
	add := func(e iso9660.Entry) {
		if e.IsDir || seen[e.Path] {
			return
		}
		seen[e.Path] = true
		plan.files = append(plan.files, e)
		plan.bytes += e.Size
	}
	for _, p := range paths {
		e, err := fsys.Stat(p)
		if err != nil {
			return plan, fmt.Errorf("%s: %w", p, err)
		}
		if !e.IsDir {
			add(e)
			continue
		}
		if err := fsys.Walk(e.Path, func(child iso9660.Entry) error {
			add(child)
			return nil
		}); err != nil {
			return plan, err
		}
	}
	plan.single = len(paths) == 1 && len(plan.files) == 1 && plan.files[0].Path == path.Clean("/"+strings.TrimPrefix(paths[0], "/"))
	plan.root = commonParent(paths)
	return plan, nil
}

// commonParent is the directory the chosen paths sit in, which is the part
// of them the archive does not need. One path gives its own parent; several
// give the deepest directory they share; anything chosen at the root of the
// disc gives nothing to trim.
func commonParent(paths []string) string {
	var parent []string
	for i, p := range paths {
		clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
		dir := strings.TrimPrefix(path.Dir(clean), "/")
		parts := []string{}
		if dir != "" && dir != "." {
			parts = strings.Split(dir, "/")
		}
		if i == 0 {
			parent = parts
			continue
		}
		if len(parts) < len(parent) {
			parent = parent[:len(parts)]
		}
		for j := range parent {
			if parts[j] != parent[j] {
				parent = parent[:j]
				break
			}
		}
	}
	return strings.Join(parent, "/")
}

// ripFiles pulls files off the disc into the store. One file is written as
// itself; several become a tar, because the store is a flat directory of
// names and a tar is the one format that carries a tree through it and is
// unpacked by everything.
// fsys is passed in rather than fetched here: by the time this runs the job
// holds the drive, and reading the volume descriptors would be refused as a
// borrow of a drive that is in use.
func (s *server) ripFiles(ctx context.Context, rec *jobRecord, d *drive, base string, chosen bool, fsys discfs.FS, plan filePlan, format archiveFormat, speedKB int) error {
	if err := s.checkRoom(plan.bytes); err != nil {
		return err
	}

	dev, err := d.open()
	if err != nil {
		return err
	}
	if speedKB > 0 {
		if err := dev.SetSpeed(speedKB); err != nil {
			rec.say("the drive would not accept a speed limit: %v", err)
		}
	}
	if plan.single {
		e := plan.files[0]
		// One file is written as itself, under the name it has on the disc
		// unless another was asked for.
		name := safeName(path.Base(e.Path), "file")
		if chosen {
			name = withExtension(base, e.Path)
		}
		name = s.unique(name)
		rec.setTotal(e.Size)
		out, err := s.newSink(rec, name, 0)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			out.Close()
			if !committed {
				_ = s.store.Remove(name)
			}
		}()
		src, _, err := fsys.Open(e.Path)
		if err != nil {
			return err
		}
		if _, err := copyCtx(ctx, out, src, e.Size); err != nil {
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		committed = true
		rec.setHash(out.sum())
		rec.addTarget(name)
		return nil
	}

	name := s.unique(base + format.Extension)
	rec.setTotal(plan.bytes)
	out, err := s.newSink(rec, name, 0)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		out.Close()
		if !committed {
			_ = s.store.Remove(name)
		}
	}()

	// The archive's progress is counted in the bytes read off the disc, not
	// the bytes written: a compressed archive has no length until it is
	// finished, and a bar that cannot reach its end is worse than none.
	err = writeArchive(ctx, out.plain(), format, fsys, plan, archiveProgress{
		starting: func(e iso9660.Entry) { rec.setPhase(e.Path) },
		finished: func(done int64) { rec.progress(done) },
	})
	if err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	committed = true
	rec.setHash(out.sum())
	rec.addTarget(name)
	return nil
}

// copyCtx is io.Copy that notices cancellation, and that pads a short read
// rather than writing a tar entry whose length does not match its header -
// which would corrupt every entry after it. A file that reads short is a
// file in a damaged sector, and the tar stays valid.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader, want int64) (int64, error) {
	buf := make([]byte, 256<<10)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if done+int64(n) > want {
				n = int(want - done)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return done, werr
			}
			done += int64(n)
		}
		if err == io.EOF || done >= want {
			break
		}
		if err != nil {
			break // a damaged sector: pad the rest so the length still matches
		}
	}
	if done < want {
		pad := make([]byte, 32<<10)
		for done < want {
			n := int64(len(pad))
			if rest := want - done; rest < n {
				n = rest
			}
			if _, err := dst.Write(pad[:n]); err != nil {
				return done, err
			}
			done += n
		}
	}
	return done, nil
}

// checkRoom refuses a rip that obviously will not fit. It is only a check
// where the store can answer - a local directory - and it leaves a tenth of
// a gigabyte of headroom, because filling a root filesystem completely
// costs far more than a refused rip.
func (s *server) checkRoom(need int64) error {
	free, _, ok := s.store.Space()
	if !ok || need <= 0 {
		return nil
	}
	const headroom = 100 << 20
	if free < need+headroom {
		return fmt.Errorf("this needs %s and %s has %s free",
			humanBytes(need), s.store.Describe(), humanBytes(free))
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
