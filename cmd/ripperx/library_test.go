package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kseen715/ripperX/mmc"
)

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

// Which library a request means, and what happens when it names one that is
// not there.
func TestSourceStore(t *testing.T) {
	writable := emptyStore{}
	library := readOnlyStore{emptyStore{}}
	s := &server{store: writable, isos: library}

	for _, name := range []string{"", "images"} {
		got, err := s.sourceStore(name)
		if err != nil || got != store(writable) {
			t.Errorf("source %q gave %v, %v; want the writable store", name, got, err)
		}
	}
	if got, err := s.sourceStore("isos"); err != nil || got != store(library) {
		t.Errorf("source isos gave %v, %v", got, err)
	}
	if _, err := s.sourceStore("elsewhere"); err == nil {
		t.Error("an unknown source was accepted")
	}

	// A server with no library says so rather than falling back to the
	// writable store, which would burn the wrong file.
	none := &server{store: writable}
	if _, err := none.sourceStore("isos"); !errors.Is(err, errNoISOStore) {
		t.Errorf("with no library configured, source isos gave %v", err)
	}
}

// Saying which disc an image needs is the difference between finding out
// now and finding out with a blank in the drive.
func TestDiscNeeded(t *testing.T) {
	cases := []struct {
		size int64
		want string
	}{
		{100 << 20, "CD"},
		{capacityCD80, "CD"},
		{capacityCD80 + 1, "DVD"},
		{capacityDVD, "DVD"},
		{capacityDVD + 1, "dual-layer DVD"},
		{capacityDVDDL, "dual-layer DVD"},
		{capacityDVDDL + 1, "nothing this drive writes"},
	}
	for _, tc := range cases {
		if got := discNeeded(tc.size); got != tc.want {
			t.Errorf("discNeeded(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}
