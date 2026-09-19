package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) *server {
	t.Helper()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{web: sub, authOn: true, drives: newDriveSet(nil)}
	s.jobs = newJobManager(s)
	s.api = s.routes(&auth{})
	return s
}

// The table is the only description of this server's HTTP surface, so an
// entry that is missing what the document needs would go out as a silently
// empty section.
func TestRouteTableIsComplete(t *testing.T) {
	s := testServer(t)
	seen := map[string]bool{}
	for _, rt := range s.api {
		if rt.Pattern == "" || rt.Handler == nil {
			t.Fatalf("route %q: pattern and handler are both required", rt.Pattern)
		}
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			t.Errorf("%s: registered twice; the mux would panic", key)
		}
		seen[key] = true
		switch rt.Method {
		case http.MethodGet, http.MethodPost, http.MethodDelete:
		default:
			t.Errorf("%s: method must be GET, POST or DELETE, it is enforced", key)
		}
		if rt.Summary == "" {
			t.Errorf("%s: no summary, so the docs page shows a blank heading", key)
		}
		if rt.Resp == nil && rt.Type == "" {
			t.Errorf("%s: says nothing about what it answers with", key)
		}
		if strings.Contains(rt.Pattern, "{") && rt.Param == nil {
			t.Errorf("%s: a pattern with a wildcard needs a Param, or the docs hide it", key)
		}
	}
	// The endpoints the guard lets through unauthenticated are all declared
	// here except the login page and the icon, which are static files.
	for path := range openPaths {
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		if !seen[http.MethodPost+" "+path] && !seen[http.MethodGet+" "+path] {
			t.Errorf("%s is open but not in the route table", path)
		}
	}
}

// Registering the table against a real mux is the only way to find out that
// two patterns collide: ServeMux panics rather than returning an error, and
// it would do so at startup in front of a user.
func TestRoutesRegisterWithoutPanicking(t *testing.T) {
	s := testServer(t)
	mux := http.NewServeMux()
	for _, rt := range s.api {
		mux.Handle(rt.Pattern, methodGuard(rt, rt.Handler))
	}
}

func TestOpenAPIDocumentIsGenerated(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	s.handleOpenAPI(w, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/api/drives", "/api/rip", "/api/burn", "/api/images"} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("%s is missing from the document", want)
		}
	}
}

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

// The document's paths have to be the ones the mux actually serves. The
// patterns carry Go's own wildcards, which OpenAPI spells the same way, so
// naming the parameter a second time produced "/api/drives/{id}/{id}" -
// wrong, and wrong in a way nothing but a reader would notice.
func TestDocumentedPathsMatchThePatterns(t *testing.T) {
	s := testServer(t)
	for _, rt := range s.api {
		got := rt.path()
		if strings.Contains(rt.Pattern, "{") && got != rt.Pattern {
			t.Errorf("%s is documented as %s", rt.Pattern, got)
		}
		if strings.Contains(got, "}/{") && !strings.Contains(rt.Pattern, "}/{") {
			t.Errorf("%s gained a second parameter: %s", rt.Pattern, got)
		}
		// A path the mux would never match is a path nobody can call.
		if strings.HasSuffix(got, "/") {
			t.Errorf("%s is documented with a trailing slash: %s", rt.Pattern, got)
		}
	}
}
