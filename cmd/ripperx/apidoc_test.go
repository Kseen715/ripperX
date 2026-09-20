package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
