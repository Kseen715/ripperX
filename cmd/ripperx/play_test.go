package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A browser will only play a WAV whose header says exactly what CD audio is,
// and the numbers are easy to get subtly wrong in a way that plays at the
// wrong speed rather than failing.
func TestWAVHeader(t *testing.T) {
	const data = 2352 * 100
	h := wavHeader(data)
	if len(h) != wavHeaderLen {
		t.Fatalf("header is %d bytes, want %d", len(h), wavHeaderLen)
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" || string(h[36:40]) != "data" {
		t.Fatalf("the chunk names are wrong: %q", h)
	}
	u16 := func(at int) uint16 { return binary.LittleEndian.Uint16(h[at : at+2]) }
	u32 := func(at int) uint32 { return binary.LittleEndian.Uint32(h[at : at+4]) }
	for _, tc := range []struct {
		what string
		got  uint32
		want uint32
	}{
		{"riff size", u32(4), data + wavHeaderLen - 8},
		{"fmt size", u32(16), 16},
		{"format", uint32(u16(20)), 1},
		{"channels", uint32(u16(22)), 2},
		{"sample rate", u32(24), 44100},
		{"byte rate", u32(28), 44100 * 2 * 2},
		{"block align", uint32(u16(32)), 4},
		{"bits", uint32(u16(34)), 16},
		{"data size", u32(40), data},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.what, tc.got, tc.want)
		}
	}
}

// A length that cannot fit the 32-bit field must clamp rather than wrap
// round to something small, which would truncate the track silently.
func TestWAVHeaderClamps(t *testing.T) {
	h := wavHeader(1 << 40)
	if got := binary.LittleEndian.Uint32(h[40:44]); got != 0xffffffff {
		t.Errorf("data size = %d, want it clamped", got)
	}
}

func TestContentTypeAndPlayability(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		playable bool
	}{
		{"song.MP3", "audio/mpeg", true},
		{"clip.mkv", "video/x-matroska", true},
		{"movie.VOB", "video/mpeg", true},
		{"setup.exe", "application/octet-stream", false},
		{"README", "application/octet-stream", false},
		{"readme.txt", "text/plain; charset=utf-8", false},
		{"cover.JPG", "image/jpeg", false},
		// The two that matter. A disc is a file somebody handed you, and
		// serving its index.html as text/html would run whatever is in it
		// in this server's origin, with this server's session cookie. The
		// system's MIME table knows both of these; that is exactly why it
		// is not consulted.
		{"index.html", "application/octet-stream", false},
		{"logo.svg", "application/octet-stream", false},
		{"payload.js", "application/octet-stream", false},
		{"page.xhtml", "application/octet-stream", false},
	}
	// The system's MIME table, which this must not be reading. It differs
	// from machine to machine - /etc/mime.types is a package on one distro
	// and absent on another - and this test failed in CI and passed here
	// for exactly that reason, on a build that was otherwise identical.
	mime.AddExtensionType(".exe", "application/x-msdownload")
	mime.AddExtensionType(".html", "text/html")
	mime.AddExtensionType(".svg", "image/svg+xml")

	for _, tc := range cases {
		if got := contentType(tc.name); got != tc.typ {
			t.Errorf("contentType(%q) = %q, want %q", tc.name, got, tc.typ)
		}
		if got := isPlayable(tc.name); got != tc.playable {
			t.Errorf("isPlayable(%q) = %v, want %v", tc.name, got, tc.playable)
		}
	}
}

// A header field is Latin-1 by rule, so a file named in Cyrillic cannot go
// in filename= and arrive intact. The name has to be carried in filename*,
// with an ASCII one beside it for anything that does not understand that.
func TestContentDispositionCarriesNonLatinNames(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/api/drives/sr0/file?path=/x", nil)
	if got := contentDisposition(plain, "autorun.inf"); got != `attachment; filename="autorun.inf"` {
		t.Errorf("an ASCII name = %q, want it left alone", got)
	}

	got := contentDisposition(plain, "Наклейка.txt")
	want := `attachment; filename="________.txt"; filename*=UTF-8''%D0%9D%D0%B0%D0%BA%D0%BB%D0%B5%D0%B9%D0%BA%D0%B0.txt`
	if got != want {
		t.Errorf("a Cyrillic name = %q, want %q", got, want)
	}
	// Whatever is in the name, the header must not be able to end the
	// quoted string early and add parameters of its own.
	for _, name := range []string{`a"b.iso`, "a\\b.iso", "a\rb.iso"} {
		h := contentDisposition(plain, name)
		if strings.Count(h, `"`) != 2 {
			t.Errorf("contentDisposition(%q) = %q, which does not have exactly one quoted name", name, h)
		}
	}

	inline := httptest.NewRequest(http.MethodGet, "/api/drives/sr0/file?path=/x&inline=1", nil)
	if got := contentDisposition(inline, "clip.mp4"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("an inline request = %q, want it played rather than downloaded", got)
	}
}

// A drive that is busy is 409, never 400. A client told its request was
// malformed will change the request, which is precisely the wrong thing to
// do about a drive that is occupied for the next two minutes.
func TestBusyDrivesAreAConflictNotABadRequest(t *testing.T) {
	for _, err := range []error{
		&errDriveBusy{job: "abc"},
		errDriveStreaming,
		fmt.Errorf("starting a rip: %w", &errDriveBusy{job: "abc"}),
		fmt.Errorf("starting a rip: %w", errDriveStreaming),
	} {
		if !driveUnavailable(err) {
			t.Errorf("%v was not recognised as a busy drive", err)
		}
	}
	for _, err := range []error{
		errors.New("no such rip kind \"xyz\""),
		errNoSuchDrive,
		errBadName,
	} {
		if driveUnavailable(err) {
			t.Errorf("%v was wrongly treated as a busy drive", err)
		}
	}
}
