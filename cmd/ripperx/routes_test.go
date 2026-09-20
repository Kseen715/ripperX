package main

import (
	"net/http"
	"strings"
	"testing"
)

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
