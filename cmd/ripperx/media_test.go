package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/mmc"
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

func TestMSF(t *testing.T) {
	cases := []struct {
		lba  int64
		want string
	}{
		{0, "00:00:00"},
		{74, "00:00:74"},
		{75, "00:01:00"},
		{75 * 60, "01:00:00"},
		{-5, "00:00:00"},
	}
	for _, tc := range cases {
		if got := msf(tc.lba); got != tc.want {
			t.Errorf("msf(%d) = %q, want %q", tc.lba, got, tc.want)
		}
	}
}

// userData is what makes a raw image convertible. Getting the offset wrong
// produces an .iso that is the right length and complete rubbish, so each
// sector kind is checked separately.
func TestUserDataFindsTheRightOffset(t *testing.T) {
	sync := func() []byte {
		s := make([]byte, mmc.SectorRaw)
		s[0] = 0x00
		for i := 1; i <= 10; i++ {
			s[i] = 0xff
		}
		s[11] = 0x00
		return s
	}

	mode1 := sync()
	mode1[15] = 1
	copy(mode1[16:], bytes.Repeat([]byte{0xa1}, 4))
	if got, ok := userData(mode1); !ok || got[0] != 0xa1 || len(got) != mmc.SectorData {
		t.Errorf("mode 1: ok=%v first=%#x len=%d", ok, got[0], len(got))
	}

	mode2form1 := sync()
	mode2form1[15] = 2
	mode2form1[18] = 0x00 // form 1
	copy(mode2form1[24:], bytes.Repeat([]byte{0xb2}, 4))
	if got, ok := userData(mode2form1); !ok || got[0] != 0xb2 {
		t.Errorf("mode 2 form 1: ok=%v", ok)
	}

	mode2form2 := sync()
	mode2form2[15] = 2
	mode2form2[18] = 0x20 // form 2: 2324 bytes, not part of an .iso
	if _, ok := userData(mode2form2); ok {
		t.Error("mode 2 form 2 has no 2048-byte user field and must be skipped")
	}

	audio := make([]byte, mmc.SectorRaw) // no sync pattern
	if _, ok := userData(audio); ok {
		t.Error("an audio sector has no user data field")
	}
	if _, ok := userData(make([]byte, 100)); ok {
		t.Error("a short buffer must not be read past its end")
	}
}

// A file in a damaged sector reads short. The tar entry's header has
// already been written with the full length, so the copy has to pad - or
// every entry after it in the archive is misaligned and lost.
func TestCopyCtxPadsAShortRead(t *testing.T) {
	var out bytes.Buffer
	n, err := copyCtx(context.Background(), &out, strings.NewReader("abc"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 || out.Len() != 10 {
		t.Fatalf("wrote %d bytes (n=%d), want 10", out.Len(), n)
	}
	if !bytes.Equal(out.Bytes(), append([]byte("abc"), make([]byte, 7)...)) {
		t.Errorf("the padding is not zeroes: %q", out.Bytes())
	}
}

// A source longer than the header promised must be cut, for the same reason.
func TestCopyCtxTruncatesALongRead(t *testing.T) {
	var out bytes.Buffer
	if _, err := copyCtx(context.Background(), &out, strings.NewReader("abcdefghij"), 4); err != nil {
		t.Fatal(err)
	}
	if out.String() != "abcd" {
		t.Errorf("wrote %q, want %q", out.String(), "abcd")
	}
}

func TestCopyCtxStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyCtx(ctx, io.Discard, strings.NewReader("abc"), 3); err == nil {
		t.Error("a cancelled copy must stop rather than finish")
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{512, "512 bytes"},
		{2048, "2.0 kB"},
		{700 << 20, "700.0 MB"},
		{5046586572, "4.7 GB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
