package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/Kseen715/ripperX/iso9660"
	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

// A folder taken off a disc has to arrive as one file, and which one file
// depends on where it is going: a zip opens on any machine without being
// asked twice, and the tars compress far better on the kind of thing discs
// hold. So the format is the user's choice rather than ours.
//
// Every format here is produced in this process, streaming, with nothing
// staged on disk first - which is what lets a 700 MB folder be downloaded
// straight from the disc. rar and 7z are not offered: neither has a usable
// Go writer, rar's format is proprietary and its only compressor is
// WinRAR's, and shelling out to tools that are absent on most machines
// would put two entries in this menu that fail when chosen.

// archiveFormat is one entry in the menu the page shows.
type archiveFormat struct {
	ID        string `json:"id" doc:"what to pass as the format parameter"`
	Name      string `json:"name" doc:"what to call it in a menu"`
	Extension string `json:"extension" doc:"the file extension, with its dot"`
	MediaType string `json:"mediaType" doc:"what it is served as"`
	Note      string `json:"note" doc:"when to pick this one"`
}

// archiveFormats is the menu, in the order it is worth offering. zip first
// because it is the one that needs nothing installed at the other end.
var archiveFormats = []archiveFormat{
	{"zip", "Zip", ".zip", "application/zip",
		"Opens anywhere without extra software. Compresses each file on its own, so it is the largest of these on a folder of many small files."},
	{"tar", "Tar, uncompressed", ".tar", "application/x-tar",
		"No compression at all, so it is written as fast as the disc reads. Right for a disc that is already compressed - photos, video, audio."},
	{"tar.gz", "Tar + gzip", ".tar.gz", "application/gzip",
		"The usual choice. Compresses well and quickly, and every system can open it."},
	{"tar.bz2", "Tar + bzip2", ".tar.bz2", "application/x-bzip2",
		"Smaller than gzip and markedly slower. Worth it for text and for installer discs."},
	{"tar.xz", "Tar + xz", ".tar.xz", "application/x-xz",
		"The smallest of these, and the slowest by some way. Right when the archive is going to be kept for years rather than opened tomorrow."},
}

func archiveByID(id string) (archiveFormat, bool) {
	if id == "" {
		id = "zip"
	}
	for _, f := range archiveFormats {
		if f.ID == id {
			return f, true
		}
	}
	return archiveFormat{}, false
}

type formatsResponse struct {
	Formats []archiveFormat `json:"formats" doc:"every archive format this server can produce"`
}

// archiveWriter is the little that a tar and a zip have in common: files
// go in, and at the end it is closed. The compressors sit underneath the
// tar and are closed in the right order by the implementation.
type archiveWriter interface {
	addFile(e iso9660.Entry, r io.Reader) error
	addSymlink(e iso9660.Entry) error
	Close() error
}

// newArchiveWriter builds the stack for one format. Everything streams:
// nothing is buffered beyond what the compressor itself needs.
func newArchiveWriter(format archiveFormat, w io.Writer) (archiveWriter, error) {
	if format.ID == "zip" {
		return &zipArchive{w: zip.NewWriter(w)}, nil
	}
	// The three tar variants differ only in what sits between the tar and
	// the socket.
	var (
		compressor io.WriteCloser
		err        error
	)
	switch format.ID {
	case "tar":
		compressor = nil
	case "tar.gz":
		compressor = gzip.NewWriter(w)
	case "tar.bz2":
		compressor, err = bzip2.NewWriter(w, &bzip2.WriterConfig{Level: bzip2.DefaultCompression})
	case "tar.xz":
		compressor, err = xz.NewWriter(w)
	default:
		return nil, fmt.Errorf("no such archive format %q", format.ID)
	}
	if err != nil {
		return nil, err
	}
	out := w
	if compressor != nil {
		out = compressor
	}
	return &tarArchive{tw: tar.NewWriter(out), compressor: compressor}, nil
}

type tarArchive struct {
	tw *tar.Writer
	// compressor is nil for a plain tar. It has to be closed after the tar
	// and before the caller's writer, or the last block never gets written.
	compressor io.WriteCloser
}

func (a *tarArchive) addFile(e iso9660.Entry, r io.Reader) error {
	mode := int64(0o644)
	if e.Mode&0o111 != 0 {
		mode = 0o755
	}
	if err := a.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     archiveName(e),
		Size:     e.Size,
		ModTime:  e.ModTime,
		Mode:     mode,
	}); err != nil {
		return err
	}
	// The header has already promised a length, so the copy pads or cuts to
	// match it: a file in a damaged sector must not misalign everything
	// after it in the archive.
	_, err := copyCtx(context.Background(), a.tw, r, e.Size)
	return err
}

func (a *tarArchive) addSymlink(e iso9660.Entry) error {
	return a.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     archiveName(e),
		Linkname: e.SymlinkTarget,
		ModTime:  e.ModTime,
		Mode:     0o777,
	})
}

func (a *tarArchive) Close() error {
	err := a.tw.Close()
	if a.compressor != nil {
		if e := a.compressor.Close(); err == nil {
			err = e
		}
	}
	return err
}

type zipArchive struct{ w *zip.Writer }

func (a *zipArchive) addFile(e iso9660.Entry, r io.Reader) error {
	h := &zip.FileHeader{
		Name:     archiveName(e),
		Method:   zip.Deflate,
		Modified: e.ModTime,
	}
	// A disc holds a great deal that is already compressed. Storing those
	// rather than deflating them costs nothing in size and saves the whole
	// compression pass, which on a 700 MB folder of JPEGs is most of the
	// time the download would take.
	if isPlayable(e.Name) || alreadyCompressed(e.Name) {
		h.Method = zip.Store
	}
	w, err := a.w.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = copyCtx(context.Background(), w, r, e.Size)
	return err
}

func (a *zipArchive) addSymlink(e iso9660.Entry) error {
	h := &zip.FileHeader{Name: archiveName(e), Modified: e.ModTime}
	h.SetMode(fs.ModeSymlink | 0o777)
	w, err := a.w.CreateHeader(h)
	if err != nil {
		return err
	}
	// A symlink in a zip is a file whose contents are its target; that is
	// what every unzip that understands them expects.
	_, err = io.WriteString(w, e.SymlinkTarget)
	return err
}

func (a *zipArchive) Close() error { return a.w.Close() }

// archiveName is the path an entry gets inside the archive: the disc's own
// path without its leading slash, so unpacking it produces the same tree.
func archiveName(e iso9660.Entry) string {
	return strings.TrimPrefix(e.Path, "/")
}

// alreadyCompressed lists what is not worth deflating a second time.
var compressedExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".zip": true, ".gz": true, ".bz2": true, ".xz": true, ".7z": true,
	".rar": true, ".cab": true, ".jar": true, ".apk": true, ".msi": true,
	".iso": true, ".img": true, ".flac": true,
}

func alreadyCompressed(name string) bool {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	return compressedExtensions[strings.ToLower(name[i:])]
}

// archiveProgress is how a job follows an archive being built. Both are
// optional and both are nil for a download, which has a progress bar of its
// own in the browser.
//
// The two are separate because they happen at different moments: which file
// is being read is worth showing before it is read, and how far along the
// job is can only be known after. Measuring in source bytes rather than
// bytes written is what makes the bar mean something - a compressed archive
// has no length until it is finished.
type archiveProgress struct {
	starting func(iso9660.Entry)
	finished func(sourceBytesDone int64)
}

// writeArchive puts every file of a plan into one archive. It is shared by
// the download, which writes to the response, and by the rip, which writes
// to the image store.
func writeArchive(ctx context.Context, w io.Writer, format archiveFormat, fsys *iso9660.FS, plan filePlan, p archiveProgress) error {
	ar, err := newArchiveWriter(format, w)
	if err != nil {
		return err
	}
	var done int64
	for _, e := range plan.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.starting != nil {
			p.starting(e)
		}
		if e.SymlinkTarget != "" {
			if err := ar.addSymlink(e); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		} else {
			src, _, err := fsys.Open(e.Path)
			if err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			if err := ar.addFile(e, src); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		}
		done += e.Size
		if p.finished != nil {
			p.finished(done)
		}
	}
	return ar.Close()
}

func (s *server) handleFormats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, formatsResponse{Formats: archiveFormats})
}
