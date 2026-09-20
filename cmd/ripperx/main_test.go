package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wrong verb has to be refused rather than reaching a handler that would
// read a body that is not there.
func TestMethodGuard(t *testing.T) {
	called := false
	h := methodGuard(
		route{Method: http.MethodGet, Extra: []string{http.MethodDelete}},
		func(w http.ResponseWriter, r *http.Request) { called = true },
	)
	for _, tc := range []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodHead, http.StatusOK},
		{http.MethodDelete, http.StatusOK},
		{http.MethodPost, http.StatusMethodNotAllowed},
	} {
		called = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, "/x", nil))
		if tc.want == http.StatusOK && !called {
			t.Errorf("%s: the handler was not reached", tc.method)
		}
		if tc.want != http.StatusOK && w.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.method, w.Code, tc.want)
		}
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
