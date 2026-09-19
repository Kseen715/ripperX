package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// validName is the only thing between a name from a request and a path
// joined onto the image directory or onto an SMB share. The traversals that
// matter are the ones spelled with the separator of the *other* system:
// filepath.Base on Linux passes a backslash straight through.
func TestValidNameRefusesEscapes(t *testing.T) {
	bad := []struct{ name, why string }{
		{"", "empty"},
		{".", "the current directory"},
		{"..", "the parent directory"},
		{"../secret.iso", "a traversal"},
		{`..\..\secret.iso`, "a traversal spelled for Windows"},
		{"/etc/passwd", "an absolute path"},
		{`dir\file.iso`, "a Windows separator"},
		{"dir/file.iso", "a Unix separator"},
		{".hidden", "a hidden file"},
		{"stream.iso:$DATA", "an alternate data stream"},
		{"nul\x00.iso", "a NUL"},
		{"bell\x07.iso", "a control character"},
		{"tab\there.iso", "a tab"},
		{strings.Repeat("a", 256), "longer than any filesystem allows"},
	}
	for _, tc := range bad {
		if validName(tc.name) {
			t.Errorf("validName(%q) is true; it is %s", tc.name, tc.why)
		}
	}
}

// And what it must accept: the names real files actually have. Refusing to
// list a file is not a security property - it is a disc image nobody can
// burn. A shelf of installer images includes "tiny11 23H2 x64.iso".
func TestValidNameAcceptsRealFileNames(t *testing.T) {
	good := []string{
		"disc.iso",
		"a",
		"EPSON-20260919-093233.img",
		"x_y-z+1.tar",
		"tiny11 23H2 x64.iso",
		"ru-ru_windows_11_consumer_editions_version_24h2.iso",
		"Полис.iso",
		"naïve.iso",
		"image (copy).iso",
		"a.b.c",
		strings.Repeat("a", 255),
	}
	for _, name := range good {
		if !validName(name) {
			t.Errorf("validName(%q) is false; it is a name a real file has", name)
		}
	}
}

// Whatever validName accepts has to be safe to join onto either kind of
// path. This is the property the whole check exists for, stated directly.
func TestAcceptedNamesCannotEscapeEitherKindOfPath(t *testing.T) {
	for _, name := range []string{
		"disc.iso", "tiny11 23H2 x64.iso", "Полис.iso", "a.b.c",
		"image (copy).iso", strings.Repeat("z", 255),
	} {
		if !validName(name) {
			continue
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("%q was accepted and contains a path separator", name)
		}
		if got := filepath.Join("/images", name); filepath.Dir(got) != "/images" {
			t.Errorf("%q joins to %q, which is outside the directory", name, got)
		}
		// The same, spelled the way an SMB path is.
		smb := `share\` + name
		if strings.Count(smb, `\`) != 1 {
			t.Errorf("%q adds a separator to an SMB path: %q", name, smb)
		}
	}
}

// safeName is applied to every name ripperX invents from a disc and to every
// name a client supplies, so whatever it returns has to be a name validName
// accepts - including for input that is entirely unusable.
func TestSafeNameAlwaysProducesAValidName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"EPSON", "EPSON"},
		{"Windows 98  SE", "Windows-98-SE"},
		{"../../etc/passwd", "etc-passwd"},
		{`C:\images\disc.iso`, "C-images-disc.iso"},
		{"  ..spaces..  ", "spaces"},
		// A disc labelled in Cyrillic rips to a file named in Cyrillic.
		// The share keeps names as UTF-16 and the page sends them as
		// UTF-8; the only thing that ever lost them was this function.
		{"Наклейка", "Наклейка"},
		{"Диск 2 - фильмы", "Диск-2-фильмы"},
		{"", "fallback"},
		{"...", "fallback"},
		{strings.Repeat("x", 300), strings.Repeat("x", 180)},
		// Truncation cuts on a rune boundary: half of a two-byte letter
		// is not a name, and would not survive a round trip.
		{strings.Repeat("я", 300), strings.Repeat("я", 90)},
	}
	for _, tc := range cases {
		got := safeName(tc.in, "fallback")
		if got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !validName(got) {
			t.Errorf("safeName(%q) = %q, which validName refuses", tc.in, got)
		}
	}
}

// A token in a URL is a token in a proxy log, so the endpoints that accept
// one have to stay the handful of reads a media player needs - and in
// particular must never include anything that starts a job or writes a disc.
func TestQueryTokenOnlyOnMediaReads(t *testing.T) {
	allowed := []string{
		"/api/images/disc.iso",
		"/api/drives/sr0/file",
		"/api/drives/sr0/tar",
		"/api/drives/sr0/playlist.m3u",
		"/api/drives/sr0/audio/3.wav",
	}
	for _, p := range allowed {
		if !allowsQueryToken(p) {
			t.Errorf("%s should accept a token in the query: a player has no cookie", p)
		}
	}
	refused := []string{
		"/api/rip", "/api/burn", "/api/erase", "/api/convert", "/api/upload",
		"/api/drives", "/api/drives/sr0", "/api/drives/sr0/refresh",
		"/api/drives/sr0/tray/eject", "/api/jobs/abc/cancel", "/api/state",
	}
	for _, p := range refused {
		if allowsQueryToken(p) {
			t.Errorf("%s must not accept a token in the query", p)
		}
	}
}

// An empty store has to answer with an empty list rather than a null: a
// client that has to special-case a missing field is one that will forget
// to. The SMB store returns nil for a share whose folder does not exist
// yet, which is exactly when a client is most likely to be new.
func TestEmptyLibraryIsAnEmptyList(t *testing.T) {
	s := &server{store: emptyStore{}}
	w := httptest.NewRecorder()
	s.handleLibrary(w, httptest.NewRequest(http.MethodGet, "/api/images", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Files []storedFile `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Files == nil {
		t.Errorf("files came back as null: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"files":[]`) {
		t.Errorf("files is not an empty array: %s", w.Body.String())
	}
}

// emptyStore is a store with nothing in it, which is what a share whose
// folder has yet to be created looks like.
type emptyStore struct{}

func (emptyStore) Create(string) (io.WriteCloser, error) { return nil, errNoSuchImage }
func (emptyStore) Open(string) (io.ReadSeekCloser, storedFile, error) {
	return nil, storedFile{}, errNoSuchImage
}
func (emptyStore) List() ([]storedFile, error) { return nil, nil }
func (emptyStore) Remove(string) error         { return errNoSuchImage }
func (emptyStore) Describe() string            { return "//nowhere/share" }
func (emptyStore) Kind() string                { return "smb" }
func (emptyStore) Space() (int64, int64, bool) { return 0, 0, false }

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

// Every writer closes the sink to learn whether the file landed, and then a
// deferred cleanup closes it again. On a local file that is harmless; on an
// SMB share the second close hangs, so a rip to a share wrote its file and
// then never finished. The sink has to tolerate it.
func TestSinkCloseIsIdempotent(t *testing.T) {
	counter := &countingCloser{}
	k := &sink{w: counter, hash: sha256.New(), rec: &jobRecord{}}

	if _, err := k.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Errorf("the second close returned %v, want nil", err)
	}
	if counter.closes != 1 {
		t.Errorf("the underlying file was closed %d times, want once", counter.closes)
	}
	// The hash is still whatever was written, closed or not.
	if k.sum() != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("hash came out as %s", k.sum())
	}
}

type countingCloser struct{ closes int }

func (c *countingCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingCloser) Close() error                { c.closes++; return nil }

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

// SMB reports the room on a share in allocation units, and an allocation
// unit is two numbers multiplied together. Using one of them reports a
// share as a whole multiple smaller than it is - which is not an error
// anything catches, because the answer looks perfectly reasonable.
func TestAllocationUnitIsBothNumbers(t *testing.T) {
	cases := []struct {
		bytesPerSector, sectorsPerUnit uint64
		want                           int64
	}{
		{512, 8, 4096},  // the ordinary NTFS cluster
		{2048, 2, 4096}, // what the share in front of me reports
		{4096, 1, 4096}, // sectors the size of the unit
		{512, 0, 512},   // a server that will not say
		{0, 8, 0},       // and one that says nothing usable
	}
	for _, c := range cases {
		if got := allocationUnit(c.bytesPerSector, c.sectorsPerUnit); got != c.want {
			t.Errorf("allocationUnit(%d, %d) = %d, want %d",
				c.bytesPerSector, c.sectorsPerUnit, got, c.want)
		}
	}

	// The figure that started this: 7.56 TB of share reported as 3.8 TB.
	const units = 1_977_614_336 // allocation units on //TOWER/tower-vault
	total := int64(units) * allocationUnit(2048, 2)
	if tb := float64(total) / (1 << 40); tb < 7.3 || tb > 7.7 {
		t.Errorf("a 7.56 TB share came out as %.2f TB", tb)
	}
}

// The type ripperX gives a file is the type. Without this header a browser
// may sniff the bytes instead and decide that a file off a disc beginning
// with "<html>" is a document to render - in this server's origin.
func TestEveryResponseRefusesSniffing(t *testing.T) {
	var reached bool
	h := noSniff(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/", "/api/status", "/api/drives/sr0/file?path=/x.html"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s came back with X-Content-Type-Options %q", path, got)
		}
	}
	if !reached {
		t.Error("the handler underneath was never called")
	}
}
