package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

// Playing a disc in a browser needs no transcoding and no plugin, because
// what is on an audio CD is already PCM: 44.1 kHz, 16-bit, stereo,
// little-endian, which is exactly what goes inside a WAV. So a track is
// served as a WAV header followed by the sectors themselves, read as they
// are asked for. Nothing is ripped to disk first, and a seek in the browser
// becomes a seek of the laser.
//
// A file on a data disc is served the same way, straight out of its extent,
// with byte ranges honoured - which is what lets a browser scrub through a
// video on a CD without reading the parts it skipped.

const wavHeaderLen = 44

// wavHeader is the 44-byte canonical header for CD audio: two channels,
// 44100 samples a second, 16 bits a sample.
func wavHeader(dataLen int64) []byte {
	h := make([]byte, wavHeaderLen)
	const (
		channels   = 2
		sampleRate = 44100
		bits       = 16
	)
	byteRate := sampleRate * channels * bits / 8
	copy(h[0:4], "RIFF")
	// The RIFF size counts everything after this field.
	binary.LittleEndian.PutUint32(h[4:8], uint32(clampU32(dataLen+wavHeaderLen-8)))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // size of the fmt chunk
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM, uncompressed
	binary.LittleEndian.PutUint16(h[22:24], channels)
	binary.LittleEndian.PutUint32(h[24:28], sampleRate)
	binary.LittleEndian.PutUint32(h[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:34], channels*bits/8) // block align
	binary.LittleEndian.PutUint16(h[34:36], bits)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(clampU32(dataLen)))
	return h
}

// clampU32 keeps a length that will not fit a WAV's 32-bit size field from
// wrapping round to something small. A CD cannot hold that much audio, so
// this only ever matters if something upstream has gone wrong.
func clampU32(n int64) int64 {
	if n < 0 {
		return 0
	}
	if n > 0xffffffff {
		return 0xffffffff
	}
	return n
}

// trackReader presents one audio track as a seekable WAV: the header, then
// the track's sectors read on demand. It is what http.ServeContent needs to
// answer a Range request, and it means a browser asking for the last ten
// seconds of a track reads ten seconds' worth of sectors.
type trackReader struct {
	dev    *mmc.Drive
	track  mmc.Track
	header []byte
	size   int64
	pos    int64
}

func newTrackReader(dev *mmc.Drive, t mmc.Track) *trackReader {
	data := t.Sectors * mmc.SectorRaw
	return &trackReader{
		dev:    dev,
		track:  t,
		header: wavHeader(data),
		size:   data + wavHeaderLen,
	}
}

func (t *trackReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		t.pos = off
	case io.SeekCurrent:
		t.pos += off
	case io.SeekEnd:
		t.pos = t.size + off
	default:
		return 0, errors.New("bad whence")
	}
	if t.pos < 0 {
		t.pos = 0
	}
	return t.pos, nil
}

func (t *trackReader) Read(p []byte) (int, error) {
	if t.pos >= t.size {
		return 0, io.EOF
	}
	if t.pos < wavHeaderLen {
		n := copy(p, t.header[t.pos:])
		t.pos += int64(n)
		return n, nil
	}
	// Audio reads have to start on a sector boundary, so the read is made
	// sector-aligned and the unwanted head of it is dropped.
	dataPos := t.pos - wavHeaderLen
	lba := t.track.Start + dataPos/mmc.SectorRaw
	skip := dataPos % mmc.SectorRaw

	count := mmc.ChunkRaw
	if rest := t.track.Start + t.track.Sectors - lba; rest < int64(count) {
		count = int(rest)
	}
	if count <= 0 {
		return 0, io.EOF
	}
	buf := make([]byte, count*mmc.SectorRaw)
	if _, err := t.dev.ReadRawRetry(lba, count, mmc.SectorCDDA, buf); err != nil {
		return 0, err
	}
	n := copy(p, buf[skip:])
	t.pos += int64(n)
	if t.pos > t.size {
		n -= int(t.pos - t.size)
		t.pos = t.size
	}
	return n, nil
}

// handleAudio serves one audio track as a WAV. The URL ends in .wav so that
// a player which decides by extension - VLC among them - knows what it is
// being handed before it reads a byte.
func (s *server) handleAudio(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	num, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("track"), ".wav"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the track number is not a number"})
		return
	}
	_, disc, err := d.state(5 * time.Second)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if disc == nil || !disc.Present {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "there is no disc in this drive"})
		return
	}
	var track *mmc.Track
	for i := range disc.Tracks {
		if disc.Tracks[i].Number == num && disc.Tracks[i].Audio {
			track = &disc.Tracks[i]
			break
		}
	}
	if track == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no audio track by that number on this disc"})
		return
	}

	err = d.borrow(func(dev *mmc.Drive) error {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Accept-Ranges", "bytes")
		name := fmt.Sprintf("track%02d.wav", num)
		w.Header().Set("Content-Disposition", contentDisposition(r, name))
		http.ServeContent(w, r, name, time.Time{}, newTrackReader(dev, *track))
		return nil
	})
	if err != nil {
		writeBusy(w, err)
	}
}

// handleDiscFile serves one file off the disc's filesystem, with ranges, so
// it can be downloaded or played in place.
func (s *server) handleDiscFile(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "no path was given"})
		return
	}
	fsys, err := d.filesystem()
	if err != nil {
		writeBusy(w, err)
		return
	}
	src, entry, err := fsys.Open(p)
	if err != nil {
		code := http.StatusNotFound
		if errors.Is(err, iso9660.ErrIsDir) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, errorResponse{Error: err.Error()})
		return
	}

	err = d.borrow(func(dev *mmc.Drive) error {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", contentType(entry.Name))
		w.Header().Set("Content-Disposition",
			contentDisposition(r, downloadName(r, path.Base(entry.Path))))

		out := http.ResponseWriter(w)
		if trackable(r) {
			rec, ctx := s.jobs.begin("download", d.id,
				fmt.Sprintf("%s from %s", path.Base(entry.Path), d.id), entry.Size)
			counted := &countingWriter{ResponseWriter: w, rec: rec, ctx: ctx}
			out = counted
			// A browser that goes away mid-download leaves the response
			// simply stopping, with no error to notice. Comparing what was
			// sent against what was promised is the only way to tell that
			// from a download that finished, and recording it as finished
			// would be a lie the job list then keeps.
			defer func() {
				var derr error
				if counted.n < entry.Size {
					derr = fmt.Errorf("the download stopped after %s of %s",
						humanBytes(counted.n), humanBytes(entry.Size))
				}
				s.jobs.end(rec, ctx, derr)
			}()
		}
		http.ServeContent(out, r, entry.Name, entry.ModTime, src)
		return nil
	})
	if err != nil {
		writeBusy(w, err)
	}
}

// handleDiscArchive streams a directory of the disc as one archive, in
// whichever format was asked for. Nothing is staged first: the archive is
// produced as the disc is read, so a 700 MB folder starts downloading
// immediately and never lands on this machine's disk.
func (s *server) handleDiscArchive(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	format, ok := archiveByID(r.URL.Query().Get("format"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("no such archive format %q", r.URL.Query().Get("format"))})
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "/"
	}
	fsys, err := d.filesystem()
	if err != nil {
		writeBusy(w, err)
		return
	}
	plan, err := planFiles(fsys, []string{p})
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}

	name := requestedName(r, "")
	if name == "" {
		name = safeName(strings.TrimPrefix(path.Clean(p), "/"), "")
	}
	if name == "" {
		name = safeName(fsys.Volume().VolumeID, "disc")
	}
	err = d.borrow(func(dev *mmc.Drive) error {
		w.Header().Set("Content-Type", format.MediaType)
		w.Header().Set("Content-Disposition", contentDisposition(r, name+format.Extension))

		// Measured in the bytes read off the disc, not the bytes sent: a
		// compressed archive has no length until it is finished.
		rec, jobCtx := s.jobs.begin("download", d.id,
			fmt.Sprintf("%s%s from %s", name, format.Extension, d.id), plan.bytes)
		ctx, stop := joinContexts(r.Context(), jobCtx)
		defer stop()

		// The length cannot be known in advance for a compressed format, and
		// for an uncompressed one it would still be a promise this could not
		// keep if a sector turned out to be unreadable partway through.
		aerr := writeArchive(ctx, w, format, fsys, plan, archiveProgress{
			starting: func(e iso9660.Entry) { rec.setPhase(e.Path) },
			finished: func(done int64) { rec.progress(done) },
		})
		if aerr != nil {
			// The response has already started, so the only honest thing left
			// is to stop: the archive ends short and the download shows as
			// incomplete.
			log.Printf("archive of %s from %s ended early: %v", p, d.id, aerr)
		}
		s.jobs.end(rec, jobCtx, aerr)
		return nil
	})
	if err != nil {
		writeBusy(w, err)
	}
}

// handlePlaylist writes an m3u naming everything playable on the disc, with
// absolute URLs, so it can be handed to VLC or any other player. When this
// server asks for a login, a token is put in each URL: a player is not a
// browser and has no cookie to send.
func (s *server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	_, disc, err := d.state(5 * time.Second)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if disc == nil || !disc.Present {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "there is no disc in this drive"})
		return
	}

	base := externalBase(r)
	token := tokenFrom(r, sessionCookie)
	withToken := func(u string) string {
		if token == "" {
			return u
		}
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		return u + sep + "access_token=" + url.QueryEscape(token)
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\r\n")
	n := 0
	for _, t := range disc.Tracks {
		if !t.Audio {
			continue
		}
		n++
		fmt.Fprintf(&b, "#EXTINF:%d,Track %02d\r\n", int(t.DurationSeconds), t.Number)
		fmt.Fprintf(&b, "%s\r\n", withToken(fmt.Sprintf("%s/api/drives/%s/audio/%d.wav", base, d.id, t.Number)))
	}
	if disc.DataTracks > 0 {
		if fsys, err := d.filesystem(); err == nil {
			_ = fsys.Walk("/", func(e iso9660.Entry) error {
				if e.IsDir || !isPlayable(e.Name) {
					return nil
				}
				n++
				fmt.Fprintf(&b, "#EXTINF:-1,%s\r\n", path.Base(e.Path))
				fmt.Fprintf(&b, "%s\r\n", withToken(fmt.Sprintf(
					"%s/api/drives/%s/file?path=%s", base, d.id, url.QueryEscape(e.Path))))
				return nil
			})
		}
	}
	if n == 0 {
		writeJSON(w, http.StatusNotFound,
			errorResponse{Error: "there is nothing playable on this disc"})
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", d.id+".m3u"))
	_, _ = io.WriteString(w, b.String())
}

// externalBase is the address a player should come back to. A reverse
// proxy's forwarded headers are believed when present, because otherwise
// every URL in the playlist would point at the private address this
// process happens to be bound to.
// joinContexts is done when either is: the browser going away, or Stop
// being pressed on the job.
func joinContexts(a, b context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-ctx.Done():
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

func externalBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return scheme + "://" + host
}

// contentDisposition decides whether a browser plays a file or saves it.
// The page asks for one or the other explicitly, because guessing is what
// produces a video that downloads instead of playing.
// requestedName is the name the caller asked the result be called. It is a
// name, not a path: it goes through safeName like every other name ripperX
// invents, so a request cannot choose where its own download lands.
func requestedName(r *http.Request, fallback string) string {
	return safeName(r.URL.Query().Get("name"), fallback)
}

// downloadName keeps the extension the file has when a new name is given
// without one, so renaming "VTS_01_1.VOB" to "opening scene" still produces
// something a player will open.
func downloadName(r *http.Request, actual string) string {
	want := requestedName(r, "")
	if want == "" {
		return actual
	}
	if path.Ext(want) == "" {
		want += path.Ext(actual)
	}
	return want
}

func contentDisposition(r *http.Request, name string) string {
	kind := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		kind = "inline"
	}
	// A header field is Latin-1 by rule, so a name with Cyrillic in it -
	// or Greek, or a Japanese track title - cannot go in filename= and
	// arrive intact. RFC 5987 carries it in filename*, percent-encoded
	// UTF-8, which every browser prefers when it is there. The plain
	// filename= stays beside it, folded to ASCII, for whatever does not.
	if ascii := asciiName(name); ascii != name {
		return fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s", kind, ascii, percentEncode(name))
	}
	return fmt.Sprintf("%s; filename=%q", kind, name)
}

// asciiName is the fallback name: the same name with everything a header
// cannot carry replaced, so it is still recognisable rather than empty.
func asciiName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f, r > 0x7e, r == '"', r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// percentEncode writes a name as RFC 5987 wants it: UTF-8, with everything
// outside the small set of characters a header value may hold spelled out
// in percent escapes.
func percentEncode(name string) string {
	const safe = "!#$&+-.^_`|~"
	var b strings.Builder
	for _, c := range []byte(name) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// playableTypes are the media formats a browser will open without help.
// Everything else is offered as a download and as a line in the playlist,
// where VLC can have it instead.
var playableTypes = map[string]string{
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".wav": "audio/wav", ".flac": "audio/flac", ".ogg": "audio/ogg",
	".oga": "audio/ogg", ".opus": "audio/ogg", ".wma": "audio/x-ms-wma",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".ogv": "video/ogg", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".dat": "video/mpeg",
	".mov": "video/quicktime", ".wmv": "video/x-ms-wmv", ".vob": "video/mpeg",
}

func isPlayable(name string) bool {
	_, ok := playableTypes[strings.ToLower(path.Ext(name))]
	return ok
}

// viewableTypes are the other things worth naming: a browser can show them
// in place, and none of them is a document that can run script in this
// server's origin.
//
// What is deliberately absent is as important as what is here. There is no
// .html, no .svg, no .js: a disc is a file somebody handed you, and serving
// its index.html as text/html would run whatever is in it inside ripperX's
// own origin, with ripperX's own session cookie. Those arrive as bytes, and
// a browser offered bytes downloads them.
var viewableTypes = map[string]string{
	".txt": "text/plain; charset=utf-8",
	".nfo": "text/plain; charset=utf-8",
	".log": "text/plain; charset=utf-8",
	".md":  "text/plain; charset=utf-8",
	".cue": "text/plain; charset=utf-8",
	".ini": "text/plain; charset=utf-8",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp",
	".pdf": "application/pdf",
}

// contentType is what a file on the disc is served as.
//
// Only types named here are used. The system's table - mime.TypeByExtension
// - is deliberately not consulted, for two reasons. It answers differently
// on different machines, because it reads /etc/mime.types: the same disc
// served from two servers would come back as two different types, and a
// test of this function passed on one machine and failed on another. And it
// knows about .html and .svg, which are the two things a disc must never be
// served as.
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := playableTypes[ext]; ok {
		return t
	}
	if t, ok := viewableTypes[ext]; ok {
		return t
	}
	return "application/octet-stream"
}

// driveUnavailable reports an error that means "not now" rather than "not
// ever": the request is perfectly good, the drive is simply busy with
// something else. Those are 409, never 400 - a client told its request was
// malformed will change the request, which is exactly the wrong response to
// a drive that is occupied for the next two minutes.
func driveUnavailable(err error) bool {
	var busy *errDriveBusy
	return errors.As(err, &busy) || errors.Is(err, errDriveStreaming)
}

// writeBusy turns the drive-in-use errors into the one status code that
// means it, and everything else into a server error.
func writeBusy(w http.ResponseWriter, err error) {
	if driveUnavailable(err) {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// A download of a whole file, or of a folder as an archive, holds the drive
// for as long as it takes and is the only thing ripperX does that did not
// show up anywhere. It is a job like any other now: it appears in the list
// with its progress, and Stop on the page ends the response.
//
// Two things are deliberately not tracked. A range request is a player
// seeking rather than a download, and one file being scrubbed through would
// otherwise fill the list with a job per seek. An inline request is a
// browser playing the file in place, which is the same thing by another
// name.
func trackable(r *http.Request) bool {
	return r.Header.Get("Range") == "" && r.URL.Query().Get("inline") != "1"
}

// countingWriter reports what has been sent so far, and stops the response
// when the job is cancelled. Everything else is the real ResponseWriter, so
// headers and status codes are untouched.
type countingWriter struct {
	http.ResponseWriter
	rec *jobRecord
	ctx context.Context
	n   int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.ResponseWriter.Write(p)
	if n > 0 {
		c.n += int64(n)
		c.rec.progress(c.n)
	}
	return n, err
}
