package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHLSPlaylistCoversTheWholeFilm(t *testing.T) {
	// Twenty seconds at six a segment is three full ones and a stub.
	body := hlsPlaylist(20, 6, func(n int) string { return fmt.Sprintf("%d.ts", n) })
	for _, want := range []string{
		"#EXTM3U", "#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-TARGETDURATION:6", "#EXT-X-ENDLIST",
		"#EXTINF:6.000,\n0.ts", "#EXTINF:2.000,\n3.ts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the playlist has no %q in it:\n%s", want, body)
		}
	}
	if n := strings.Count(body, ".ts"); n != 4 {
		t.Errorf("the playlist names %d segments, want 4", n)
	}
	// A film shorter than one segment is still one segment, not none.
	if n := strings.Count(hlsPlaylist(2, 6, func(int) string { return "0.ts" }), ".ts"); n != 1 {
		t.Errorf("a two-second film came out as %d segments, want 1", n)
	}
}

// pal is a DVD's picture: 720 columns stored, 768 meant, 576 lines.
var pal = sourceFormat{width: 720, height: 576, sarNum: 16, sarDen: 15}

func TestSegmentArgsPlaceTheSegmentInTheFilm(t *testing.T) {
	args := strings.Join(segmentArgs("http://127.0.0.1:8998/api/internal/source/abc", 17, 6,
		originalRung(pal.height), pal), " ")
	// Seventeen segments in: read from 102 seconds, and say so on the way
	// out, or the player stitches the pieces on top of each other.
	for _, want := range []string{
		"-ss 102.000", "-t 6.000", "-output_ts_offset 102.000",
		"-i http://127.0.0.1:8998/api/internal/source/abc", "-f mpegts pipe:1",
		"yadif=deint=interlaced", "setsar=1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the command line has no %q in it:\n%s", want, args)
		}
	}
	if first := strings.Join(segmentArgs("src", 0, 6, originalRung(pal.height), pal), " "); !strings.Contains(first, "-ss 0.000") {
		t.Errorf("the first segment does not start at the beginning:\n%s", first)
	}

	// The disc's own size is left alone; every rung below it is scaled, to
	// a width that keeps the shape and that an encoder will accept.
	if strings.Contains(args, "flags=bicubic") {
		t.Errorf("the disc's own size was scaled anyway:\n%s", args)
	}
	small, err := rungByHeight(pal, 360)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.Join(segmentArgs("src", 0, 6, small, pal), " ")
	for _, want := range []string{"scale=480:360:flags=bicubic", "-crf 28", "-maxrate 1200k", "-b:a 96k"} {
		if !strings.Contains(low, want) {
			t.Errorf("360p has no %q in it:\n%s", want, low)
		}
	}
}

// The ladder is the sizes below the disc's own, and never above it: a DVD
// offered at 1080p would be the same picture, blown up, at four times the
// bitrate.
func TestLadderNeverScalesUp(t *testing.T) {
	var heights []int
	for _, r := range rungsFor(pal) {
		heights = append(heights, r.height)
	}
	if fmt.Sprint(heights) != "[144 240 360 480 576]" {
		t.Errorf("a PAL DVD is offered at %v, want 144p to 480p and its own 576", heights)
	}
	if w := rungsFor(pal)[len(heights)-1].width(pal); w != 768 {
		t.Errorf("the disc's own size is %dx576, want 768 wide - its shape, not its storage", w)
	}

	// A source that is already small offers only what is below it.
	small := sourceFormat{width: 320, height: 240, sarNum: 1, sarDen: 1}
	heights = nil
	for _, r := range rungsFor(small) {
		heights = append(heights, r.height)
	}
	if fmt.Sprint(heights) != "[144 240]" {
		t.Errorf("a 240-line source is offered at %v, want 144p and its own 240", heights)
	}
	if _, err := rungByHeight(small, 720); err == nil {
		t.Error("a size nobody offered was accepted")
	}
}

// The master playlist is what makes the menu in the player: one line per
// size, with the shape the picture is meant to be shown in.
func TestMasterPlaylistNamesEverySize(t *testing.T) {
	body := hlsMaster(rungsFor(pal), pal, func(r rung) string {
		return fmt.Sprintf("level.m3u8?title=2&height=%d", r.height)
	})
	for _, want := range []string{
		"RESOLUTION=192x144", "RESOLUTION=640x480", "RESOLUTION=768x576",
		`NAME="144p"`, `NAME="576p"`,
		"level.m3u8?title=2&height=360",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the playlist has no %q in it:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "#EXT-X-STREAM-INF"); n != 5 {
		t.Errorf("the playlist offers %d sizes, want 5", n)
	}
}

func TestNonceIsOnePerTitleAndForgotten(t *testing.T) {
	store := newNonceStore()
	first, err := store.issue("sr0", "/VIDEO_TS/VTS_01_1.VOB")
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.issue("sr0", "/VIDEO_TS/VTS_01_1.VOB")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("watching one film minted two names for it")
	}
	other, err := store.issue("sr0", "/VIDEO_TS/VTS_02_1.VOB")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two titles were given the same name")
	}

	ref, ok := store.lookup(first)
	if !ok || ref.drive != "sr0" || ref.source != "/VIDEO_TS/VTS_01_1.VOB" {
		t.Fatalf("the name resolved to %+v, %v", ref, ok)
	}
	if _, ok := store.lookup("not a name anybody issued"); ok {
		t.Error("a name nobody issued was accepted")
	}

	// A film abandoned halfway through loses its name, and a name that has
	// expired is no longer a way into the disc.
	store.byID[first].seen = time.Now().Add(-2 * nonceTTL)
	if _, ok := store.lookup(first); ok {
		t.Error("a name that had gone quiet for half an hour still worked")
	}
	if _, ok := store.byKey["sr0\x00/VIDEO_TS/VTS_01_1.VOB"]; ok {
		t.Error("the expired name was left behind in the index")
	}
}

func TestSourceIsRefusedFromAnywhereElse(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/internal/source/abc", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	if !loopbackOnly(r) {
		t.Error("this server's own encoder was refused")
	}
	r.RemoteAddr = "10.18.18.44:54321"
	if loopbackOnly(r) {
		t.Error("a request from another machine was let in")
	}
}

func TestSelfHostIsSomewhereThisServerAnswers(t *testing.T) {
	for addr, want := range map[string]string{
		"0.0.0.0:8998":   "127.0.0.1:8998",
		":8998":          "127.0.0.1:8998",
		"[::]:8998":      "127.0.0.1:8998",
		"192.168.1.5:80": "192.168.1.5:80",
	} {
		if got := selfHost(addr); got != want {
			t.Errorf("listening on %s, this server reaches itself at %s, want %s", addr, got, want)
		}
	}
}

func TestTranscodableNeedsAnEncoderAndSomethingToEncode(t *testing.T) {
	s := &server{}
	if s.transcodable("VTS_01_1.VOB") {
		t.Error("a server with no ffmpeg offered to convert something")
	}
	s.ffmpeg = &transcoder{ffmpeg: "ffmpeg", ffprobe: "ffprobe"}
	for name, want := range map[string]bool{
		"VTS_01_1.VOB": true, "clip.avi": true, "song.flac": true,
		"README.TXT": false, "disc.iso": false, "cover.jpg": false,
	} {
		if got := s.transcodable(name); got != want {
			t.Errorf("%s: transcodable is %v, want %v", name, got, want)
		}
	}
}

func TestBrowserPlaysOnlyWhatItActuallyPlays(t *testing.T) {
	// The bug this whole path exists for: a VOB is a file ripperX knows the
	// type of, and a file no browser has ever played.
	if playsInBrowser("VTS_01_1.VOB") {
		t.Error("a DVD VOB is still offered to the browser as something it can play")
	}
	if !isPlayable("VTS_01_1.VOB") {
		t.Error("a VOB dropped out of the playlist VLC is given")
	}
	for _, name := range []string{"clip.mp4", "song.mp3", "clip.webm"} {
		if !playsInBrowser(name) {
			t.Errorf("%s is no longer played by the browser itself", name)
		}
	}
}

// The endpoints refuse before they do anything expensive: a server with no
// ffmpeg says so rather than starting something it cannot finish, and a
// segment that is not a number never reaches the drive.
func TestHLSEndpointsRefuseEarly(t *testing.T) {
	s := testServer(t)
	s.drives = newDriveSet([]string{"/dev/sr0"})

	req := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.SetPathValue("id", "sr0")
		if strings.Contains(path, "/hls/") && !strings.Contains(path, "index.m3u8") {
			r.SetPathValue("seg", strings.TrimPrefix(path[strings.Index(path, "/hls/")+5:], ""))
		}
		w := httptest.NewRecorder()
		if strings.Contains(path, "index.m3u8") {
			s.handleHLSPlaylist(w, r)
		} else {
			s.handleHLSSegment(w, r)
		}
		return w
	}

	if w := req("/api/drives/sr0/hls/index.m3u8?path=/VIDEO_TS/VTS_01_1.VOB"); w.Code != http.StatusNotFound {
		t.Errorf("with no ffmpeg the playlist is %d, want 404", w.Code)
	} else if !strings.Contains(w.Body.String(), "ffmpeg") {
		t.Errorf("the refusal does not mention ffmpeg: %s", w.Body.String())
	}

	s.ffmpeg = &transcoder{ffmpeg: "ffmpeg", ffprobe: "ffprobe"}
	if w := req("/api/drives/sr0/hls/index.m3u8"); w.Code != http.StatusBadRequest {
		t.Errorf("a playlist with no path is %d, want 400", w.Code)
	}
	if w := req("/api/drives/sr0/hls/not-a-number.ts?path=/x.vob"); w.Code != http.StatusBadRequest {
		t.Errorf("a segment that is not a number is %d, want 400", w.Code)
	}
}

// The encoder's door is not a way into the drives for anybody else.
func TestInternalSourceIsClosedToTheWorld(t *testing.T) {
	s := testServer(t)
	s.drives = newDriveSet([]string{"/dev/sr0"})
	nonce, err := s.nonces.issue("sr0", "/VIDEO_TS/VTS_01_1.VOB")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/internal/source/"+nonce, nil)
	r.SetPathValue("nonce", nonce)
	r.RemoteAddr = "10.18.18.44:1234"
	w := httptest.NewRecorder()
	s.handleInternalSource(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("a request from another machine is %d, want 404", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/internal/source/whatever", nil)
	r.SetPathValue("nonce", "whatever")
	r.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	s.handleInternalSource(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("a name nobody issued is %d, want 404", w.Code)
	}
}

// The one test that runs the real programs. Everything above checks what is
// asked of ffmpeg; this checks that ffmpeg answers it - that a segment out
// of the middle of a film is six seconds of something a browser plays, and
// that it carries the timestamps of its place in the film rather than of
// its own beginning. Get that last part wrong and every segment starts at
// zero, which a player stitches into six seconds of film and a scrub bar
// that goes nowhere.
//
// It is skipped where ffmpeg is not installed, which is where the feature
// is absent anyway.
func TestSegmentIsPlayableVideoAtTheRightPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("this one runs ffmpeg")
	}
	tc := findTranscoder("")
	if tc == nil {
		t.Skip("no ffmpeg on this machine")
	}
	ctx := t.Context()

	// Something shaped like what comes off a DVD: MPEG-2 video and AC-3
	// audio in a program stream, which is the pair no browser decodes.
	src := filepath.Join(t.TempDir(), "fake.vob")
	make := exec.CommandContext(ctx, tc.ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=720x576:rate=25:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:v", "mpeg2video", "-b:v", "3000k", "-c:a", "ac3", "-t", "30",
		"-f", "mpeg", src)
	if out, err := make.CombinedOutput(); err != nil {
		t.Skipf("this build of ffmpeg could not make the test file: %v: %s", err, out)
	}

	f, err := tc.probeFormat(ctx, src)
	if err != nil {
		t.Fatalf("probing the file: %v", err)
	}
	if f.duration < 29 || f.duration > 31 {
		t.Errorf("the film is %.2f seconds long, want about 30", f.duration)
	}
	if f.width != 720 || f.height != 576 {
		t.Errorf("the picture is %dx%d, want 720x576", f.width, f.height)
	}

	// The third segment: twelve seconds in, six seconds long.
	seg, err := tc.segment(ctx, src, 2, originalRung(f.height), f)
	if err != nil {
		t.Fatalf("encoding the segment: %v", err)
	}
	out := filepath.Join(t.TempDir(), "2.ts")
	if err := os.WriteFile(out, seg, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(ctx, tc.ffprobe, "-v", "error",
		"-show_entries", "format=start_time,duration:stream=codec_name",
		"-of", "default=noprint_wrappers=1", out)
	facts, err := probe.Output()
	if err != nil {
		t.Fatalf("probing the segment: %v", err)
	}
	got := string(facts)
	for _, want := range []string{"codec_name=h264", "codec_name=aac"} {
		if !strings.Contains(got, want) {
			t.Errorf("the segment has no %s in it:\n%s", want, got)
		}
	}
	for _, f := range []struct {
		key       string
		low, high float64
	}{
		{"start_time", 11.5, 12.5},
		{"duration", 5.5, 6.5},
	} {
		v, ok := probeFloat(got, f.key)
		if !ok {
			t.Errorf("the segment does not say its %s:\n%s", f.key, got)
			continue
		}
		if v < f.low || v > f.high {
			t.Errorf("the segment's %s is %.2f, want between %.1f and %.1f", f.key, v, f.low, f.high)
		}
	}
}

func probeFloat(out, key string) (float64, bool) {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || k != key {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// Asking for the same segment twice while it is being made must wait for
// the one encode rather than start a second.
//
// This is not a nicety. A player that has waited longer than it likes gives
// up and asks again; if asking again started a rival encode on the same
// drive, the two would fight over the head, both would be slower, and the
// player would give up again. That is a loop a film never comes out of, and
// it is what a DVD did until the encodes were shared.
func TestOneSegmentIsEncodedOnceHoweverOftenItIsAsked(t *testing.T) {
	s := testServer(t)
	s.drives = newDriveSet([]string{"/dev/sr0"})
	// Anything that writes to stdout and exits stands in for the encoder
	// here: what is under test is the sharing, not the encoding.
	s.ffmpeg = &transcoder{ffmpeg: "/bin/echo", ffprobe: "/bin/echo"}
	d, err := s.drives.get("sr0")
	if err != nil {
		t.Fatal(err)
	}
	m := mediaSource{title: 2}

	const askers = 8
	got := make([][]byte, askers)
	errs := make([]error, askers)
	var wg sync.WaitGroup
	for i := range askers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = s.segmentFor(t.Context(), d, m, 4, originalRung(pal.height), pal)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("asker %d: %v", i, err)
		}
		if len(got[i]) == 0 {
			t.Fatalf("asker %d got nothing", i)
		}
		// The same bytes, and the same bytes in memory: a second encode
		// would have produced its own.
		if &got[i][0] != &got[0][0] {
			t.Errorf("asker %d was given a segment of its own, so it was encoded twice", i)
		}
	}
	if _, ok := d.media.segment(m.key()+"@576", 4); !ok {
		t.Error("the segment was not kept, so asking again would encode it again")
	}
	// And the encode that finished must not be left in the way of the next.
	d.media.mu.Lock()
	left := len(d.media.inflight)
	d.media.mu.Unlock()
	if left != 0 {
		t.Errorf("%d encodes were left marked as running", left)
	}
}
