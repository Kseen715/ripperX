package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"

	"github.com/Kseen715/ripperX/discfs"
	"github.com/Kseen715/ripperX/mmc"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A DVD is MPEG-2, and no browser has decoded MPEG-2 for years. The file is
// served correctly, the <video> element accepts it, and then nothing ever
// happens - which is exactly what a disc full of VOBs looks like on the
// page.
//
// So it is transcoded on the way out, as HLS: the film is divided into
// segments of a fixed length, and each one is encoded by its own ffmpeg
// when it is asked for. That is what makes seeking work. A player jumping
// to the ninety-minute mark asks for the segment at the ninety-minute mark,
// ffmpeg seeks the input to it, and one segment later there is a picture -
// rather than an hour and a half of decoding nobody wants to watch.
//
// Nothing is staged on disk. The drive is read as the film is watched, and
// a segment already produced is kept in memory, so scrubbing backwards
// costs nothing and the laser is not sent over the same ground twice.

// segmentSeconds is how long each piece is. Six seconds is the usual
// compromise: short enough that a seek lands quickly, long enough that the
// per-segment cost of starting ffmpeg stays a small share of the work.
const segmentSeconds = 6.0

// segmentTimeout bounds one segment's encode. A drive that has stopped
// answering must not hold a slot for ever.
const segmentTimeout = 3 * time.Minute

// transcodeSlots is how many ffmpeg processes one drive may keep busy. One,
// because the limit is the drive and not the processor: two encoders
// reading two places on one disc spend their time moving the head between
// them, and both then take longer than either would alone. Measured on a
// DVD that is the difference between four seconds a segment and a player
// that gives up waiting.
const transcodeSlots = 1

// transcoder is the pair of programs this needs, found once at startup.
// Without them the feature is simply absent: nothing fails, the page offers
// a download instead of a play button.
type transcoder struct {
	ffmpeg  string
	ffprobe string
}

// findTranscoder locates ffmpeg and its prober. An explicit path names
// ffmpeg; ffprobe is looked for beside it, because a build installed by
// hand keeps the two together.
func findTranscoder(explicit string) *transcoder {
	ffmpeg := explicit
	if ffmpeg == "" {
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil
		}
		ffmpeg = p
	} else if _, err := exec.LookPath(ffmpeg); err != nil {
		return nil
	}
	probe := strings.TrimSuffix(ffmpeg, "ffmpeg") + "ffprobe"
	if _, err := exec.LookPath(probe); err != nil {
		p, err := exec.LookPath("ffprobe")
		if err != nil {
			return nil
		}
		probe = p
	}
	return &transcoder{ffmpeg: ffmpeg, ffprobe: probe}
}

// probeDuration asks how long the film is. It is asked of the same URL the
// encoder will read, so the answer covers the whole title rather than the
// first of its parts.
//
// A program stream does not record its length, so ffprobe works it out from
// the timestamps at both ends - which needs a seek to the end of the disc
// and is why the answer is remembered.
func (t *transcoder) probeDuration(ctx context.Context, src string) (float64, error) {
	cmd := exec.CommandContext(ctx, t.ffprobe,
		"-v", "error",
		"-show_entries", "format=duration,bit_rate",
		"-of", "default=noprint_wrappers=1",
		src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe could not read this file: %s",
			firstLine(stderr.String(), err.Error()))
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "duration" {
			continue
		}
		d, err := strconv.ParseFloat(value, 64)
		if err == nil && d > 0 {
			return d, nil
		}
	}
	return 0, errors.New("this file does not say how long it is")
}

// firstLine is the one line of ffmpeg's complaint worth repeating.
func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

// hlsPlaylist is the whole film as a list of segments. It is a VOD
// playlist: the length is known, every segment is named, and the player is
// free to ask for them in any order - which is what seeking is.
func hlsPlaylist(duration, segLen float64, segURL func(n int) string) string {
	count := int(math.Ceil(duration / segLen))
	if count < 1 {
		count = 1
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(segLen)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for n := range count {
		length := segLen
		// The last one is whatever is left, and saying so is what keeps the
		// end of the scrub bar where the end of the film is.
		if rest := duration - float64(n)*segLen; rest < segLen {
			length = rest
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", length, segURL(n))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// segmentArgs is the command line for one segment.
//
// Each segment is encoded on its own, so it has to start with a keyframe
// and carry the timestamps of its place in the film: -output_ts_offset is
// what makes segments produced by different processes join without a gap.
//
// The picture needs two corrections on the way out. DVD video is usually
// interlaced, and yadif is told to touch only the frames that say they are.
// And it is anamorphic - 720x576 shown as 16:9 - so it is scaled to square
// pixels here rather than left to a browser that may or may not read the
// aspect ratio out of a transport stream.
func segmentArgs(src string, n int, segLen float64) []string {
	start := float64(n) * segLen
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		"-i", src,
		"-t", strconv.FormatFloat(segLen, 'f', 3, 64),
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-vf", "yadif=deint=interlaced,scale=iw*sar:ih,setsar=1",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-pix_fmt", "yuv420p",
		"-force_key_frames", "expr:gte(t,n_forced*" + strconv.FormatFloat(segLen, 'f', 3, 64) + ")",
		"-c:a", "aac", "-b:a", "160k", "-ac", "2",
		"-output_ts_offset", strconv.FormatFloat(start, 'f', 3, 64),
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "mpegts", "pipe:1",
	}
}

// segment encodes one piece and hands back its bytes.
func (t *transcoder) segment(ctx context.Context, src string, n int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, segmentTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffmpeg, segmentArgs(src, n, segmentSeconds)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffmpeg could not encode this part: %s",
			firstLine(stderr.String(), err.Error()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg produced nothing for this part")
	}
	return out, nil
}

// The encoder reads the disc through this server, because it has to be able
// to seek and the disc is not a file anywhere: ripperX reads its sectors
// itself, and a pipe cannot be seeked. Serving it over the loopback gives
// ffmpeg byte ranges through the same code a browser downloading the file
// would use.
//
// That request cannot carry the user's own token. A command line is
// readable by every account on the machine through /proc, so what goes
// there is a name that means nothing anywhere else: a random one, good for
// one title, for as long as it is being watched, and only from this
// machine.

// nonceTTL is how long a source name lasts without being used. It is
// renewed on every read, so a film being watched keeps its name and one
// abandoned halfway through loses it.
const nonceTTL = 15 * time.Minute

type sourceRef struct {
	drive string
	// source is a mediaSource's key: a file on the disc, or a title of it.
	source string
	seen   time.Time
}

type nonceStore struct {
	mu    sync.Mutex
	byID  map[string]*sourceRef
	byKey map[string]string
}

func newNonceStore() *nonceStore {
	return &nonceStore{byID: map[string]*sourceRef{}, byKey: map[string]string{}}
}

// issue names a title, reusing the name already given to it so that
// watching a film does not mint one per segment.
func (n *nonceStore) issue(driveID, source string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.expireLocked()
	key := driveID + "\x00" + source
	if id, ok := n.byKey[key]; ok {
		n.byID[id].seen = time.Now()
		return id, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	n.byID[id] = &sourceRef{drive: driveID, source: source, seen: time.Now()}
	n.byKey[key] = id
	return id, nil
}

// lookup resolves a name, and refuses one that has gone quiet for longer
// than a film would.
func (n *nonceStore) lookup(id string) (sourceRef, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.expireLocked()
	ref, ok := n.byID[id]
	if !ok {
		return sourceRef{}, false
	}
	ref.seen = time.Now()
	return *ref, true
}

func (n *nonceStore) expireLocked() {
	cutoff := time.Now().Add(-nonceTTL)
	for id, ref := range n.byID {
		if ref.seen.Before(cutoff) {
			delete(n.byID, id)
			delete(n.byKey, ref.drive+"\x00"+ref.source)
		}
	}
}

// loopbackOnly reports whether a request came from this machine. The source
// endpoint answers nothing else: it exists for the encoder this server
// started, and a name that leaked is still no use from another host.
func loopbackOnly(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sourceURL is the address the encoder reads a title at.
func (s *server) sourceURL(d *drive, m mediaSource) (string, error) {
	id, err := s.nonces.issue(d.id, m.key())
	if err != nil {
		return "", err
	}
	return "http://" + s.selfHost + "/api/internal/source/" + id, nil
}

// selfHost is where this server can reach itself. A server bound to every
// address is asked for on the loopback, which is the one address it
// certainly answers on and the one the encoder should use.
func selfHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// mediaSource names what is being played: a file on the disc, or a title
// the disc's own index says is inside its files. The two are asked for the
// same way everywhere below, because everything after opening them - the
// playlist, the segments, the encoder's own reads - is identical.
type mediaSource struct {
	path  string
	title int
}

func sourceFromQuery(q url.Values) (mediaSource, error) {
	if t := q.Get("title"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 {
			return mediaSource{}, errors.New("the title number is not a number")
		}
		return mediaSource{title: n}, nil
	}
	if p := q.Get("path"); p != "" {
		return mediaSource{path: p}, nil
	}
	return mediaSource{}, errors.New("no path or title was given")
}

// sourceFromKey reads back what key wrote, which is how the encoder's own
// request says what it is reading without a path in the URL.
func sourceFromKey(key string) (mediaSource, error) {
	if len(key) < 2 {
		return mediaSource{}, errors.New("that is not a source")
	}
	switch key[0] {
	case 'p':
		return mediaSource{path: key[1:]}, nil
	case 't':
		n, err := strconv.Atoi(key[1:])
		if err != nil || n < 1 {
			return mediaSource{}, errors.New("that is not a title number")
		}
		return mediaSource{title: n}, nil
	}
	return mediaSource{}, errors.New("that is not a source")
}

// key identifies the source for as long as this disc is in the drive: it is
// what durations and encoded segments are remembered under.
func (m mediaSource) key() string {
	if m.title > 0 {
		return "t" + strconv.Itoa(m.title)
	}
	return "p" + m.path
}

func (m mediaSource) query() string {
	if m.title > 0 {
		return url.Values{"title": {strconv.Itoa(m.title)}}.Encode()
	}
	return url.Values{"path": {m.path}}.Encode()
}

func (m mediaSource) name() string {
	if m.title > 0 {
		return fmt.Sprintf("title%02d.vob", m.title)
	}
	return path.Base(m.path)
}

// openSource opens whichever of the two it is.
func (s *server) openSource(d *drive, m mediaSource) (discfs.File, discfs.Entry, error) {
	fsys, err := d.filesystem()
	if err != nil {
		return nil, discfs.Entry{}, err
	}
	if m.title == 0 {
		return fsys.Open(m.path)
	}
	disc, err := d.dvd()
	if err != nil {
		return nil, discfs.Entry{}, err
	}
	for _, t := range disc.Titles {
		if t.Number != m.title {
			continue
		}
		src, size, err := openDVDTitle(fsys, t)
		if err != nil {
			return nil, discfs.Entry{}, err
		}
		name := m.name()
		return src, discfs.Entry{Name: name, Path: "/" + name, Size: size}, nil
	}
	return nil, discfs.Entry{}, fmt.Errorf("this disc has no title %d", m.title)
}

// sourceDuration is how long the thing being played lasts.
//
// A DVD title needs no probing: the disc records its length, and that is
// the only figure that is right on a disc whose files hold several titles
// one after another. Anything else is asked of ffprobe once and remembered.
func (s *server) sourceDuration(ctx context.Context, d *drive, m mediaSource, src string) (float64, error) {
	if m.title > 0 {
		disc, err := d.dvd()
		if err != nil {
			return 0, err
		}
		for _, t := range disc.Titles {
			if t.Number == m.title {
				if t.Seconds <= 0 {
					return 0, errors.New("this disc does not say how long this title is")
				}
				return t.Seconds, nil
			}
		}
		return 0, fmt.Errorf("this disc has no title %d", m.title)
	}
	if d, ok := d.media.duration(m.key()); ok {
		return d, nil
	}
	seconds, err := s.ffmpeg.probeDuration(ctx, src)
	if err != nil {
		return 0, err
	}
	d.media.setDuration(m.key(), seconds)
	return seconds, nil
}

// transcodable says a file is worth offering a play button for even though
// the browser cannot open it: there is an encoder, and it is something with
// a picture or a sound in it.
func (s *server) transcodable(name string) bool {
	if s.ffmpeg == nil {
		return false
	}
	t := contentType(name)
	return strings.HasPrefix(t, "video/") || strings.HasPrefix(t, "audio/")
}

// handleHLSPlaylist hands the player the shape of the film: how long it is
// and what to ask for. Producing it costs one probe of the disc, which is
// remembered until the disc changes.
func (s *server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	d, m, err := s.transcodeTarget(w, r)
	if err != nil {
		return
	}
	src, err := s.sourceURL(d, m)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	duration, err := s.sourceDuration(r.Context(), d, m, src)
	if err != nil {
		if driveUnavailable(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		return
	}

	query := m.query()
	body := hlsPlaylist(duration, segmentSeconds, func(n int) string {
		return fmt.Sprintf("%d.ts?%s", n, query)
	})
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// handleHLSSegment encodes one piece of the film, or hands back the one it
// encoded a moment ago.
func (s *server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	d, m, err := s.transcodeTarget(w, r)
	if err != nil {
		return
	}
	n, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("seg"), ".ts"))
	if err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "that is not a segment number"})
		return
	}
	seg, err := s.segmentFor(r.Context(), d, m, n)
	if err != nil {
		if r.Context().Err() != nil {
			// The player moved on - it seeked, or the page was closed.
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeSegment(w, seg)
}

// segmentFor is one segment, encoded if it has to be, and encoded once
// however many times it is asked for.
//
// Both of those matter more than they look. A player that has waited longer
// than it likes gives up and asks again, and if asking again cancelled the
// encode and started another, the segment would never be finished: every
// request would kill the work the next one waits for. So the encode is
// detached from the request that started it - it runs to the end, into the
// cache, whether or not anybody is still listening - and a second request
// for the same segment waits for the first rather than starting a rival.
func (s *server) segmentFor(ctx context.Context, d *drive, m mediaSource, n int) ([]byte, error) {
	if seg, ok := d.media.segment(m.key(), n); ok {
		return seg, nil
	}
	src, err := s.sourceURL(d, m)
	if err != nil {
		return nil, err
	}
	key := segmentKey(m.key(), n)
	job, mine := d.media.beginSegment(key)
	if mine {
		go s.encodeSegment(d, m, n, src, key, job)
	}
	select {
	case <-job.done:
		return job.seg, job.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) encodeSegment(d *drive, m mediaSource, n int, src, key string, job *segmentJob) {
	ctx, cancel := context.WithTimeout(context.Background(), segmentTimeout)
	defer cancel()
	// One encoder per drive, and a queue for the rest: a disc read in two
	// places at once is read slowly in both.
	select {
	case d.transcodes <- struct{}{}:
	case <-ctx.Done():
		d.media.finishSegment(key, nil, ctx.Err())
		return
	}
	seg, err := s.ffmpeg.segment(ctx, src, n)
	<-d.transcodes
	if err == nil {
		d.media.setSegment(m.key(), n, seg)
	}
	d.media.finishSegment(key, seg, err)
}

func writeSegment(w http.ResponseWriter, seg []byte) {
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(seg)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(seg)
}

// transcodeTarget is the checking both HLS endpoints do: a drive that
// exists, an encoder to use, and a path on a disc that can be read. It
// writes the refusal itself, so a handler only has to stop.
func (s *server) transcodeTarget(w http.ResponseWriter, r *http.Request) (*drive, mediaSource, error) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return nil, mediaSource{}, err
	}
	if s.ffmpeg == nil {
		err := errors.New("this server has no ffmpeg, so it cannot convert anything for a browser")
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return nil, mediaSource{}, err
	}
	m, err := sourceFromQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return nil, mediaSource{}, err
	}
	return d, m, nil
}

// handleInternalSource is the encoder's door. It is not part of the API a
// person uses: it takes no path of its own, only a name issued a moment ago
// for a title on a disc, and it answers nobody outside this machine.
func (s *server) handleInternalSource(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(r) {
		http.NotFound(w, r)
		return
	}
	ref, ok := s.nonces.lookup(r.PathValue("nonce"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := s.drives.get(ref.drive)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := sourceFromKey(ref.source)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	src, entry, err := s.openSource(d, m)
	if err != nil {
		if driveUnavailable(err) {
			writeBusy(w, err)
			return
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	err = d.borrow(func(*mmc.Drive) error {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", contentType(entry.Name))
		http.ServeContent(w, r, entry.Name, entry.ModTime, src)
		return nil
	})
	if err != nil {
		writeBusy(w, err)
	}
}
