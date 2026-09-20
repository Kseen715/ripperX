package main

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/Kseen715/ripperX/discfs"
	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
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
	// Playable says the browser will open this file itself, and
	// Transcodable that this server can convert it into something the
	// browser will open. Between them they decide whether the page offers a
	// play button, and which kind of player it opens.
	Playable     bool `json:"playable" doc:"a browser can play this file in place"`
	Transcodable bool `json:"transcodable" doc:"this server can convert it for the browser on the fly"`
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
			be.Playable = playsInBrowser(e.Name)
			be.Transcodable = s.transcodable(e.Name)
			be.Type = contentType(e.Name)
		}
		resp.Entries = append(resp.Entries, be)
	}
	writeJSON(w, http.StatusOK, resp)
}

// How big a folder is, is a question a listing cannot answer. A file's size
// is in its own directory record and costs nothing; a folder's is the sum of
// everything under it, which means reading every directory record in the
// tree - one seek each, on the slowest storage still in use. On a disc with
// a deep tree that is seconds, and doing it for every row of every listing
// would make browsing unusable.
//
// So it is asked for rather than given: the page shows a button, and this
// answers it. Several folders can be asked about at once, because the
// natural thing to want is the whole listing and one request holding the
// drive once is kinder to it than twenty.

type dirSizesResponse struct {
	Sizes []dirSize `json:"sizes" doc:"one answer per path asked about, in the order they were asked"`
}

type dirSize struct {
	Path  string `json:"path" doc:"the directory that was measured"`
	Bytes int64  `json:"bytes" doc:"the total size of every file under it"`
	Files int64  `json:"files" doc:"how many files that was"`
	Dirs  int64  `json:"dirs" doc:"how many directories are under it"`
	Error string `json:"error,omitempty" doc:"why this one could not be measured, when it could not"`
}

// maxSizePaths bounds one request. A listing is a screenful; anything asking
// about a thousand directories at once is not a page.
const maxSizePaths = 256

func (s *server) handleDirSizes(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	if len(paths) > maxSizePaths {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf(
			"%d directories were asked about at once; %d is the most", len(paths), maxSizePaths)})
		return
	}
	fsys, err := d.filesystem()
	if err != nil {
		if driveUnavailable(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		return
	}

	resp := dirSizesResponse{Sizes: make([]dirSize, 0, len(paths))}
	// The whole walk happens under one borrow: a job that wants the drive is
	// told so rather than left to interleave its seeks with these.
	err = d.borrow(func(*mmc.Drive) error {
		for _, p := range paths {
			if err := r.Context().Err(); err != nil {
				return err
			}
			resp.Sizes = append(resp.Sizes, measureDir(fsys, p))
		}
		return nil
	})
	if err != nil {
		writeBusy(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func measureDir(fsys discfs.FS, p string) dirSize {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	out := dirSize{Path: clean}
	e, err := fsys.Stat(clean)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if !e.IsDir {
		out.Bytes, out.Files = e.Size, 1
		return out
	}
	if err := fsys.Walk(clean, func(child iso9660.Entry) error {
		if child.IsDir {
			out.Dirs++
			return nil
		}
		out.Files++
		out.Bytes += child.Size
		return nil
	}); err != nil {
		out.Error = err.Error()
	}
	return out
}
