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
	"sync"
	"time"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
)

// The library is the flat set of files in the image store: what rips wrote,
// and what was uploaded to be burned. It is the one place a disc and a
// browser meet - a rip lands here and is downloaded from here, an upload
// lands here and is burned from here - so both directions go through the
// same names and the same validation.

type libraryResponse struct {
	Files []libraryFile `json:"files" doc:"every image in the store, newest first"`
	Store string        `json:"store" doc:"where they are, with no password in it"`
	Kind  string        `json:"kind" doc:"local or smb"`
	Free  int64         `json:"free,omitempty" doc:"bytes free where images are written, when that can be established"`
	// ReadOnly marks the library nothing can be written to, so a page does
	// not offer a delete button it would only be refused for.
	ReadOnly bool `json:"readOnly,omitempty" doc:"true for the read-only image library"`
}

func (s *server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := libraryResponse{Files: plainFiles(files), Store: s.store.Describe(), Kind: s.store.Kind()}
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

// Two places an image can come from: the writable store, where rips land
// and uploads go, and the read-only library of installer images. A request
// names which, and a request that names neither means the writable one -
// which is what every client written before the library existed sends.
const (
	sourceImages = "images"
	sourceISOs   = "isos"
)

var errNoISOStore = errors.New("this server has no read-only image library configured")

// sourceStore picks the store a request means.
func (s *server) sourceStore(name string) (store, error) {
	switch name {
	case "", sourceImages:
		return s.store, nil
	case sourceISOs:
		if s.isos == nil {
			return nil, errNoISOStore
		}
		return s.isos, nil
	}
	return nil, fmt.Errorf("no such image source %q; it is %s or %s", name, sourceImages, sourceISOs)
}

func (s *server) handleISOs(w http.ResponseWriter, r *http.Request) {
	if s.isos == nil {
		writeJSON(w, http.StatusOK, libraryResponse{Files: []libraryFile{}, Kind: "none"})
		return
	}
	files, err := s.isos.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, libraryResponse{
		Files:    s.isoFacts.fill(r.Context(), s.isos, files),
		Store:    s.isos.Describe(),
		Kind:     s.isos.Kind(),
		ReadOnly: true,
	})
}

// Disc capacities, for saying which disc an image needs rather than leaving
// someone to find out when a 6 GB image will not fit a 4.7 GB blank.
const (
	capacityCD80  = 737280000  // an 80-minute CD-R
	capacityDVD   = 4700372992 // single layer
	capacityDVDDL = 8547991552 // dual layer
)

// discNeeded says the smallest disc this image will fit on.
func discNeeded(size int64) string {
	switch {
	case size <= capacityCD80:
		return "CD"
	case size <= capacityDVD:
		return "DVD"
	case size <= capacityDVDDL:
		return "dual-layer DVD"
	default:
		return "nothing this drive writes"
	}
}

// libraryFile is one image in a library, and - where it is worth the read -
// what is inside it.
//
// Choosing which disc to spend is choosing between an image that boots on
// the machine in front of you and one that does not, and that is not in the
// file name: half a shelf of installer images is named after a version and
// nothing else. So the ISO library fills these in. The image store does
// not, because there a listing would mean opening every rip on every page
// load to learn nothing anyone asked for.
type libraryFile struct {
	storedFile
	Sectors int64             `json:"sectors,omitempty" doc:"how many 2048-byte sectors it is"`
	Aligned bool              `json:"aligned,omitempty" doc:"whether it is a whole number of sectors, which a disc image always is"`
	Needs   string            `json:"needs,omitempty" doc:"the smallest disc it will fit on"`
	Volume  string            `json:"volume,omitempty" doc:"what the ISO 9660 volume calls itself"`
	Boot    *iso9660.BootInfo `json:"boot,omitempty" doc:"which firmware and which architectures a disc written from it will boot"`
	Summary string            `json:"summary,omitempty" doc:"one line about this image, for a person"`
	Error   string            `json:"error,omitempty" doc:"why the image could not be read as one"`
}

// plainFiles lists files without looking inside them. An empty store is an
// empty list, never a null: a client that has to special-case the absence
// of a field is a client that will forget to.
func plainFiles(files []storedFile) []libraryFile {
	out := make([]libraryFile, 0, len(files))
	for _, f := range files {
		out = append(out, describeSize(f))
	}
	return out
}

// describeSize fills in what the size alone says: whether the file could be
// a disc image at all, and the smallest disc it would fit on. It costs no
// reads, so every listing gets it.
func describeSize(f storedFile) libraryFile {
	return libraryFile{
		storedFile: f,
		Sectors:    f.Size / mmc.SectorData,
		Aligned:    f.Size > 0 && f.Size%mmc.SectorData == 0,
		Needs:      discNeeded(f.Size),
	}
}

type imageInfoResponse struct {
	libraryFile
	Source string `json:"source" doc:"images or isos"`
}

// handleImageInfo says what an image is before anyone spends a disc on it:
// whether it is really an ISO, what disc it needs, and - the question
// nobody can answer by looking at the file - whether the result will boot,
// on what firmware, and for which processor.
func (s *server) handleImageInfo(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	src, err := s.sourceStore(source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	name := r.URL.Query().Get("name")
	if !validName(name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: errBadName.Error()})
		return
	}
	f, info, err := src.Open(name)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errNoSuchImage) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, errorResponse{Error: err.Error()})
		return
	}
	defer f.Close()
	writeJSON(w, http.StatusOK, imageInfoResponse{
		libraryFile: inspectImage(f, info),
		Source:      cmp(source, sourceImages),
	})
}

// inspectImage reads the few hundred bytes that say what an image is.
func inspectImage(f io.ReadSeeker, info storedFile) libraryFile {
	out := describeSize(info)
	at := seekReaderAt{f}
	if fsys, err := iso9660.Open(at); err == nil {
		out.Volume = fsys.Volume().VolumeID
	}
	boot, err := iso9660.ReadBootInfo(at)
	switch {
	case errors.Is(err, iso9660.ErrNotISO9660):
		out.Error = "this file is not an ISO 9660 image. It may still be a disc image of some " +
			"other kind, but ripperX cannot tell, and a disk image meant for a USB stick " +
			"written to a disc will not boot."
	case err != nil:
		out.Error = err.Error()
	default:
		out.Boot = boot
	}
	out.Summary = describeImage(out)
	return out
}

func cmp(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func describeImage(r libraryFile) string {
	if r.Error != "" {
		return r.Error
	}
	line := fmt.Sprintf("%s, needs a %s", humanBytes(r.Size), r.Needs)
	if !r.Aligned {
		return line + ". It is not a whole number of 2048-byte sectors, so it is not a disc image and cannot be burned."
	}
	if r.Volume != "" {
		line += fmt.Sprintf(", volume %q", r.Volume)
	}
	if r.Boot != nil {
		line += ". " + upperFirst(r.Boot.Summary())
	}
	return line + "."
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// factCache remembers what was found inside each image, so that listing a
// library of sixty installer images does not read sixty images every time
// the page loads. A file is identified by its name, size and modification
// time, so a file that changes is looked at again and a file that is gone
// stops being remembered.
type factCache struct {
	mu sync.Mutex
	m  map[string]libraryFile
}

func newFactCache() *factCache { return &factCache{m: map[string]libraryFile{}} }

func factKey(f storedFile) string {
	return fmt.Sprintf("%s|%d|%d", f.Name, f.Size, f.ModTime.UnixNano())
}

// factWorkers is how many images are looked at concurrently. The reads are
// small but there are two or three round trips in each, and over a share
// that is latency rather than work - so several at once, and not so many
// that a listing becomes a burst of connections to somebody's NAS.
const factWorkers = 6

// fill looks inside every file it has not already looked inside.
//
// It stops when the request does: an image that could not be read in time
// is listed without its facts and looked at on the next pass. A slower
// answer is better than no listing, and much better than a page that hangs
// because a share went away.
func (c *factCache) fill(ctx context.Context, src store, files []storedFile) []libraryFile {
	out := make([]libraryFile, len(files))
	var todo []int
	c.mu.Lock()
	for i, f := range files {
		if known, ok := c.m[factKey(f)]; ok {
			out[i] = known
			continue
		}
		out[i] = describeSize(f)
		todo = append(todo, i)
	}
	c.mu.Unlock()

	work := make(chan int)
	var wg sync.WaitGroup
	for range min(factWorkers, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				f, info, err := src.Open(files[i].Name)
				if err != nil {
					continue
				}
				found := inspectImage(f, info)
				f.Close()
				c.mu.Lock()
				c.m[factKey(files[i])] = found
				out[i] = found
				c.mu.Unlock()
			}
		}()
	}
	for _, i := range todo {
		select {
		case work <- i:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()

	c.forget(files)
	return out
}

// forget drops what was remembered about files that are no longer there, so
// the cache cannot grow without bound on a library that is being replaced a
// file at a time.
func (c *factCache) forget(files []storedFile) {
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[factKey(f)] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if !keep[k] {
			delete(c.m, k)
		}
	}
}
