package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kseen715/ripperX/mmc"
)

// The library is the flat set of files in the image store: what rips wrote,
// and what was uploaded to be burned. It is the one place a disc and a
// browser meet - a rip lands here and is downloaded from here, an upload
// lands here and is burned from here - so both directions go through the
// same names and the same validation.

type libraryResponse struct {
	Files []storedFile `json:"files" doc:"every image in the store, newest first"`
	Store string       `json:"store" doc:"where they are, with no password in it"`
	Kind  string       `json:"kind" doc:"local or smb"`
	Free  int64        `json:"free,omitempty" doc:"bytes free where images are written, when that can be established"`
}

func (s *server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// An empty store is an empty list, never a null: a client that has to
	// special-case the absence of a field is a client that will forget to.
	if files == nil {
		files = []storedFile{}
	}
	resp := libraryResponse{Files: files, Store: s.store.Describe(), Kind: s.store.Kind()}
	if free, ok := s.store.FreeBytes(); ok {
		resp.Free = free
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleImage serves one image out of the store, or deletes it. Ranges are
// honoured, so a half-finished download of a 4 GB image resumes rather than
// starting again.
func (s *server) handleImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: errBadName.Error()})
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.Remove(name); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, errNoSuchImage) {
				code = http.StatusNotFound
			}
			writeJSON(w, code, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, statusMessage{Status: "deleted " + name})
		return
	}

	f, info, err := s.store.Open(name)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errNoSuchImage) || errors.Is(err, errBadName) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, errorResponse{Error: err.Error()})
		return
	}
	defer f.Close()
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Content-Disposition", contentDisposition(r, name))
	http.ServeContent(w, r, name, info.ModTime, f)
}

type uploadResponse struct {
	File   storedFile `json:"file" doc:"what was stored"`
	SHA256 string     `json:"sha256" doc:"SHA-256 of the bytes received, to check against the sender's own"`
}

// handleUpload takes an image from the browser into the store, so a disc
// can be burned from a file that was never on this machine. The body is a
// multipart form with one file part, which is what a plain <input
// type=file> sends and what curl -F sends.
//
// The hash of what arrived is returned: an upload that was truncated by a
// proxy or a dropped connection is otherwise indistinguishable from a
// complete one until the burn fails.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.uploadMax)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			errorResponse{Error: "this endpoint takes a multipart form with one file in it"})
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		resp, code, err := s.storeUpload(r, part)
		part.Close()
		if err != nil {
			writeJSON(w, code, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}
	writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the form had no file in it"})
}

func (s *server) storeUpload(r *http.Request, part *multipart.Part) (uploadResponse, int, error) {
	// The name a browser sends is whatever the file was called on the other
	// machine, including its directory on some browsers. Only the base name
	// is taken, and then only after being made safe.
	base := path.Base(filepath.ToSlash(part.FileName()))
	name := safeName(base, "")
	if name == "" {
		name = "upload-" + time.Now().Format("20060102-150405")
	}
	if requested := r.URL.Query().Get("name"); requested != "" {
		if n := safeName(requested, ""); n != "" {
			name = n
		}
	}

	f, err := s.store.Create(name)
	if err != nil {
		return uploadResponse{}, http.StatusInternalServerError, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), part)
	closeErr := f.Close()
	if err != nil {
		_ = s.store.Remove(name)
		// MaxBytesReader's error is the one worth translating: it is the
		// difference between "your file is too big" and "something broke".
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return uploadResponse{}, http.StatusRequestEntityTooLarge,
				fmt.Errorf("this file is larger than the %s this server accepts", humanBytes(s.uploadMax))
		}
		return uploadResponse{}, http.StatusInternalServerError, err
	}
	if closeErr != nil {
		_ = s.store.Remove(name)
		return uploadResponse{}, http.StatusInternalServerError, closeErr
	}
	return uploadResponse{
		File:   storedFile{Name: name, Size: n, ModTime: time.Now()},
		SHA256: hex.EncodeToString(sum.Sum(nil)),
	}, http.StatusCreated, nil
}

type convertRequest struct {
	Name string `json:"name" doc:"the raw .img in the store to convert"`
}

// handleConvert turns a raw 2352-byte-per-sector image into a 2048-byte
// .iso by taking the user data out of each sector. It exists because a raw
// image is the faithful copy but an .iso is the one that can be mounted and
// burned, and having ripped the first there is no reason to go back to the
// disc for the second.
//
// Only Mode 1 and Mode 2 Form 1 sectors carry 2048 bytes of user data at a
// fixed offset. An audio sector carries none, so a mixed disc converts to
// an .iso of its data track and nothing else - which is exactly what an
// .iso of a mixed disc means.
func (s *server) handleConvert(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if !validName(req.Name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: errBadName.Error()})
		return
	}
	src, info, err := s.store.Open(req.Name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	size := info.Size
	src.Close()
	if size%mmc.SectorRaw != 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("%s is %d bytes, which is not a whole number of 2352-byte sectors, so it is not a raw image", req.Name, size)})
		return
	}
	out := strings.TrimSuffix(req.Name, filepath.Ext(req.Name)) + ".iso"

	job := s.jobs.start("convert", "", fmt.Sprintf("%s to %s", req.Name, out),
		size/mmc.SectorRaw*mmc.SectorData,
		func(ctx context.Context, rec *jobRecord) error {
			return s.convertImage(ctx, rec, req.Name, out)
		})
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

func (s *server) convertImage(ctx context.Context, rec *jobRecord, from, to string) error {
	src, info, err := s.store.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := s.checkRoom(info.Size / mmc.SectorRaw * mmc.SectorData); err != nil {
		return err
	}

	dst, err := s.newSink(rec, to, 0)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		dst.Close()
		if !committed {
			_ = s.store.Remove(to)
		}
	}()

	const batch = 64
	buf := make([]byte, batch*mmc.SectorRaw)
	data := make([]byte, batch*mmc.SectorData)
	skipped := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(src, buf)
		if n == 0 {
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		sectors := n / mmc.SectorRaw
		kept := 0
		for i := range sectors {
			sec := buf[i*mmc.SectorRaw : (i+1)*mmc.SectorRaw]
			user, ok := userData(sec)
			if !ok {
				skipped++
				continue
			}
			copy(data[kept*mmc.SectorData:], user)
			kept++
		}
		if kept > 0 {
			if _, err := dst.Write(data[:kept*mmc.SectorData]); err != nil {
				return err
			}
		}
		if sectors < batch {
			break
		}
	}
	if err := dst.Close(); err != nil {
		return err
	}
	committed = true
	rec.setHash(dst.sum())
	rec.addTarget(to)
	if skipped > 0 {
		rec.say("%d sectors were not data sectors and were left out", skipped)
	}
	return nil
}

// userData finds the 2048 bytes of user data inside a raw sector. Mode 1
// puts them at offset 16; Mode 2 Form 1 puts them at 24, after the
// subheader. Anything else - audio, Mode 2 Form 2 - has no 2048-byte user
// field and is not part of an .iso.
func userData(sector []byte) ([]byte, bool) {
	if len(sector) < mmc.SectorRaw {
		return nil, false
	}
	// A data sector starts with the sync pattern 00 FF*10 00.
	if sector[0] != 0x00 || sector[11] != 0x00 {
		return nil, false
	}
	for _, b := range sector[1:11] {
		if b != 0xff {
			return nil, false
		}
	}
	switch sector[15] { // the mode byte of the sector header
	case 1:
		return sector[16 : 16+mmc.SectorData], true
	case 2:
		// Form 2 is flagged in the subheader; it carries 2324 bytes and no
		// error correction, so it is not part of an .iso.
		if sector[18]&0x20 != 0 {
			return nil, false
		}
		return sector[24 : 24+mmc.SectorData], true
	}
	return nil, false
}
