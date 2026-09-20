package main

import "testing"

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
