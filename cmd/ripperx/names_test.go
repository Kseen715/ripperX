package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// validName is the only thing between a name from a request and a path
// joined onto the image directory or onto an SMB share, so the traversals
// that matter are the ones spelled with the separator of the *other*
// system: filepath.Base on Linux passes a backslash straight through.
func TestValidNameRefusesEscapes(t *testing.T) {
	bad := []string{
		"", ".", "..", "../secret", `..\..\secret.iso`, "/etc/passwd",
		`dir\file.iso`, "dir/file.iso", ".hidden", "with space.iso",
		"nul\x00.iso", "naïve.iso", strings.Repeat("a", 201),
	}
	for _, name := range bad {
		if validName(name) {
			t.Errorf("validName(%q) is true, it must not be", name)
		}
	}
	good := []string{"disc.iso", "a", "EPSON-20260919-093233.img", "x_y-z+1.tar", "A.B.C"}
	for _, name := range good {
		if !validName(name) {
			t.Errorf("validName(%q) is false, it should be a usable name", name)
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
		{"Наклейка", "fallback"},
		{"", "fallback"},
		{"...", "fallback"},
		{strings.Repeat("x", 300), strings.Repeat("x", 180)},
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
func (emptyStore) FreeBytes() (int64, bool)    { return 0, false }

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
