package main

import (
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/Kseen715/ripperX/iso9660"
)

// Browsing a disc reads its directory records where they lie, which is why
// a listing costs one seek rather than a copy of the disc. Every entry
// comes back with what the page needs to decide what to offer for it: its
// size, whether it can be played in the browser, and the path to ask for it
// by.

type browseResponse struct {
	Path    string         `json:"path" doc:"the directory that was listed"`
	Parent  string         `json:"parent,omitempty" doc:"the directory above it, or empty at the root"`
	Volume  iso9660.Volume `json:"volume" doc:"what the disc says about itself"`
	Entries []browseEntry  `json:"entries" doc:"what is in this directory, directories first"`
}

type browseEntry struct {
	iso9660.Entry
	// Playable says the browser will open this file itself, which is what
	// decides whether the page offers a play button or only a download.
	Playable bool `json:"playable" doc:"a browser can play this file in place"`
	// Type is what it would be served as.
	Type string `json:"type" doc:"the content type this file would be served with"`
}

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	fsys, err := d.filesystem()
	if err != nil {
		if driveUnavailable(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
			return
		}
		// Anything else here is a disc this server cannot read rather than a
		// request it cannot understand.
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		return
	}

	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}
	dir = path.Clean("/" + strings.TrimPrefix(dir, "/"))

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		code := http.StatusNotFound
		if errors.Is(err, iso9660.ErrNotDir) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, errorResponse{Error: err.Error()})
		return
	}

	resp := browseResponse{Path: dir, Volume: fsys.Volume(), Entries: []browseEntry{}}
	if dir != "/" {
		resp.Parent = path.Dir(dir)
	}
	for _, e := range entries {
		be := browseEntry{Entry: e}
		if !e.IsDir {
			be.Playable = isPlayable(e.Name)
			be.Type = contentType(e.Name)
		}
		resp.Entries = append(resp.Entries, be)
	}
	writeJSON(w, http.StatusOK, resp)
}
