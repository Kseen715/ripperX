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
	"time"

	"github.com/Kseen715/ripperX/discfs"
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
	{"iso", "ISO 9660 image", ".iso", "application/x-iso9660-image",
		"A disc rather than a file about one: it mounts anywhere and it can be burned as it is, so a folder taken off a disc can go straight back onto one. No compression, and the long names are kept twice over - in Joliet and in Rock Ridge - so they survive on Windows and on Unix alike."},
}

// isoFormat is the one entry above that is not an archive at all. It is
// produced by a different writer, because an image has to know where every
// file will sit before the first byte is written, where an archive only has
// to know what comes next.
const isoFormat = "iso"

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
	addFile(name string, e iso9660.Entry, r io.Reader) error
	addSymlink(name string, e iso9660.Entry) error
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

func (a *tarArchive) addFile(name string, e iso9660.Entry, r io.Reader) error {
	mode := int64(0o644)
	if e.Mode&0o111 != 0 {
		mode = 0o755
	}
	if err := a.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
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

func (a *tarArchive) addSymlink(name string, e iso9660.Entry) error {
	return a.tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     name,
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

func (a *zipArchive) addFile(name string, e iso9660.Entry, r io.Reader) error {
	h := &zip.FileHeader{
		Name:     name,
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

func (a *zipArchive) addSymlink(name string, e iso9660.Entry) error {
	h := &zip.FileHeader{Name: name, Modified: e.ModTime}
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
// to the image store. name is what the volume of an .iso is called and is
// ignored by every other format.
func writeArchive(ctx context.Context, w io.Writer, format archiveFormat, fsys discfs.FS, plan filePlan, name string, p archiveProgress) error {
	if format.ID == isoFormat {
		return writeISO(ctx, w, fsys, plan, name, p)
	}
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
		// The name inside the archive comes from the plan, which knows
		// what was chosen: taking /Drivers/Win7 gives an archive with Win7
		// at its root, not one with the whole path to it.
		name := plan.under(e)
		if e.SymlinkTarget != "" {
			if err := ar.addSymlink(name, e); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		} else {
			src, _, err := fsys.Open(e.Path)
			if err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			if err := ar.addFile(name, e, src); err != nil {
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

// isoItems turns a plan into the list an image is built from. The
// directories come first so that a folder with nothing in it still exists on
// the disc, which is the one thing an archive of the same folder loses.
func isoItems(plan filePlan) []iso9660.Item {
	items := make([]iso9660.Item, 0, len(plan.dirs)+len(plan.files))
	for _, e := range plan.dirs {
		if name := plan.under(e); name != "" {
			items = append(items, iso9660.Item{
				Path: name, IsDir: true, ModTime: e.ModTime, Mode: e.Mode,
			})
		}
	}
	for _, e := range plan.files {
		items = append(items, iso9660.Item{
			Path:    plan.under(e),
			Size:    e.Size,
			ModTime: e.ModTime,
			Mode:    e.Mode,
			Symlink: e.SymlinkTarget,
		})
	}
	return items
}

// isoLayout plans the image without writing it, which is how its size is
// known before anything is read off the disc: a rip can refuse for want of
// room, and a download can declare a length.
func isoLayout(plan filePlan, name string) (*iso9660.Layout, error) {
	return iso9660.Plan(isoItems(plan), iso9660.Options{
		VolumeID:    isoVolumeID(name),
		SystemID:    "LINUX",
		Preparer:    "ripperX",
		Application: "ripperX",
		Created:     time.Now(),
	})
}

// isoVolumeID is the label the image carries. ISO 9660 allows 32 uppercase
// d-characters and nothing else, and a volume with no name is one nothing
// will mount by label.
func isoVolumeID(name string) string {
	stem, _ := splitExtension(name)
	var b strings.Builder
	for _, r := range strings.ToUpper(stem) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		return "RIPPERX"
	}
	return out
}

// writeISO masters the plan as an ISO 9660 image. Everything is placed
// first and written in one pass, so nothing is staged and the result can go
// straight into a socket or onto a share.
func writeISO(ctx context.Context, w io.Writer, fsys discfs.FS, plan filePlan, name string, p archiveProgress) error {
	layout, err := isoLayout(plan, name)
	if err != nil {
		return err
	}
	byPath := make(map[string]iso9660.Entry, len(plan.files))
	for _, e := range plan.files {
		byPath[plan.under(e)] = e
	}
	var done int64
	return layout.Write(ctx, w, func(it iso9660.Item) (io.Reader, error) {
		e, ok := byPath[it.Path]
		if !ok {
			return nil, fmt.Errorf("%s is not one of the files that were planned", it.Path)
		}
		if p.starting != nil {
			p.starting(e)
		}
		src, _, err := fsys.Open(e.Path)
		if err != nil {
			return nil, err
		}
		// An image is measured in the bytes read off the disc, like every
		// other format here, so the bar means the same thing whichever was
		// chosen.
		return &countingReader{r: src, done: &done, report: p.finished}, nil
	})
}

// countingReader reports progress as the bytes go past. The image writer
// copies each file itself, so this is the only place that knows how far
// through one it is.
type countingReader struct {
	r      io.Reader
	done   *int64
	report func(int64)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		*c.done += int64(n)
		if c.report != nil {
			c.report(*c.done)
		}
	}
	return n, err
}

func (s *server) handleFormats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, formatsResponse{Formats: archiveFormats})
}
