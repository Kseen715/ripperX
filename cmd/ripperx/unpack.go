package main

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// Reading archives, so that a disc ripped into one can be written back out
// as a disc.
//
// A rip of a disc's files produces one archive, which is the right thing to
// keep: one name, one checksum, one file on the share. But it is the wrong
// thing to burn - a disc with a single .zip on it is not the disc that was
// ripped. So a burn from an archive unpacks it first and writes what was
// inside, which is what closes the loop.
//
// Only the formats ripperX itself writes are read. A .rar or a .7z would
// need a library or an external program, and the archives this has to read
// back are the ones it made.

// archiveExtensions maps what a file is called to how it is opened. The
// longer suffixes have to be tested first: ".tar.gz" ends with ".gz".
var archiveExtensions = []struct {
	suffix string
	tar    bool
	wrap   func(io.Reader) (io.Reader, error)
}{
	{suffix: ".zip"},
	{suffix: ".tar", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return r, nil }},
	{suffix: ".tar.gz", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }},
	{suffix: ".tgz", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }},
	{suffix: ".tar.xz", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return xz.NewReader(r) }},
	{suffix: ".txz", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return xz.NewReader(r) }},
	{suffix: ".tar.bz2", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }},
	{suffix: ".tbz2", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }},
	{suffix: ".tbz", tar: true, wrap: func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }},
}

// archiveOf reports how to read this name, and whether it is one at all.
func archiveOf(name string) (int, bool) {
	lower := strings.ToLower(name)
	best, found := -1, false
	for i, k := range archiveExtensions {
		if strings.HasSuffix(lower, k.suffix) {
			if !found || len(k.suffix) > len(archiveExtensions[best].suffix) {
				best, found = i, true
			}
		}
	}
	return best, found
}

func isArchive(name string) bool {
	_, ok := archiveOf(name)
	return ok
}

// unpackedFile is one file that came out of an archive: where it is under
// the staging directory, how big it is, and what it hashes to. The hash is
// taken as it is written, so verifying the disc afterwards costs one read of
// the disc and nothing else.
type unpackedFile struct {
	rel  string
	size int64
	sum  string
}

var errEmptyArchive = errors.New("there is nothing in this archive to write to a disc")

// unpackArchive extracts localPath into dest and returns what came out.
//
// Everything about the paths inside an archive is treated as hostile: they
// are whatever was in a file that may have come from anywhere, and an entry
// called "../../etc/passwd" is a real thing that real archives contain.
func unpackArchive(ctx context.Context, rec *jobRecord, name, localPath, dest string) ([]unpackedFile, int64, error) {
	idx, ok := archiveOf(name)
	if !ok {
		return nil, 0, fmt.Errorf("%s is not an archive ripperX can read; it reads %s",
			name, strings.Join(readableArchives(), ", "))
	}
	kind := archiveExtensions[idx]

	var out []unpackedFile
	var total int64
	write := func(rel string, mode fs.FileMode, r io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm(mode))
		if err != nil {
			return err
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, sum), r)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		out = append(out, unpackedFile{rel: rel, size: n, sum: hex.EncodeToString(sum.Sum(nil))})
		total += n
		if rec != nil {
			rec.progress(total)
		}
		return nil
	}
	link := func(rel, targetPath string) error {
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		// A link that points out of the tree would put a file on the disc
		// pointing at this machine's filesystem.
		if !linkStaysInside(rel, targetPath) {
			return fmt.Errorf("%s points at %q, which is outside the disc's root", rel, targetPath)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Symlink(targetPath, target)
	}

	var err error
	if kind.tar {
		err = walkTar(localPath, kind.wrap, write, link)
	} else {
		err = walkZip(localPath, write, link)
	}
	if err != nil {
		return nil, 0, err
	}
	if len(out) == 0 {
		return nil, 0, errEmptyArchive
	}
	return out, total, nil
}

func readableArchives() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range archiveExtensions {
		if !seen[k.suffix] {
			seen[k.suffix] = true
			out = append(out, k.suffix)
		}
	}
	return out
}

// filePerm keeps the executable bit and nothing else. What matters on a disc
// is whether a file can be run; the rest of a mode recorded by whatever made
// the archive is not worth carrying onto read-only media.
func filePerm(mode fs.FileMode) fs.FileMode {
	if mode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// safeJoin is the whole of the defence against a hostile archive.
//
// The obvious implementation - clean the path and join it - is wrong, and
// wrong in the direction that looks right: path.Clean("/../etc/passwd") is
// "/etc/passwd", so cleaning does not refuse a traversal, it silently
// rewrites it into a different file. That is worse than refusing. An
// archive ripperX wrote has no ".." in it, so one that does did not come
// from here and has no business being quietly corrected.
func safeJoin(dest, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, `\`, "/")
	if rel == "" || rel == "." {
		return "", errors.New("an entry in this archive has no name")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("an entry in this archive has a NUL in its name")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%q is an absolute path, which would be written outside the disc's root", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q climbs out of the archive and would be written outside the disc's root", rel)
		}
	}
	target := filepath.Join(dest, filepath.FromSlash(path.Clean(rel)))
	// Belt and braces: whatever the components said, the result has to be
	// under the destination.
	within, err := filepath.Rel(dest, target)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q would be written outside the disc's root", rel)
	}
	return target, nil
}

// linkStaysInside reports whether a symbolic link at rel, pointing at
// target, still points somewhere on the disc.
//
// Unlike a file name, a link target may legitimately contain "..": a link
// in /VIDEO_TS pointing at ../AUDIO_TS/x is an ordinary thing. So the
// components are counted rather than refused - each one that is not ".."
// goes a level deeper, each ".." comes back up, and a link that gets above
// the root at any point is one that leaves the disc.
func linkStaysInside(rel, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return false
	}
	depth := 0
	for _, part := range strings.Split(path.Dir(strings.ReplaceAll(rel, `\`, "/")), "/") {
		if part != "" && part != "." {
			depth++
		}
	}
	for _, part := range strings.Split(strings.ReplaceAll(target, `\`, "/"), "/") {
		switch part {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return false
			}
		default:
			depth++
		}
	}
	return true
}

func walkZip(localPath string, write func(string, fs.FileMode, io.Reader) error, link func(string, string) error) error {
	zr, err := zip.OpenReader(localPath)
	if err != nil {
		return fmt.Errorf("reading the archive: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			continue // directories appear again as the parents of their files
		case mode&fs.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := link(f.Name, string(target)); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = write(f.Name, mode, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func walkTar(localPath string, wrap func(io.Reader) (io.Reader, error),
	write func(string, fs.FileMode, io.Reader) error, link func(string, string) error) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := wrap(f)
	if err != nil {
		return fmt.Errorf("reading the archive: %w", err)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink:
			if err := link(hdr.Name, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := write(hdr.Name, fs.FileMode(hdr.Mode), tr); err != nil {
				return err
			}
		}
	}
}
