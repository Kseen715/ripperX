package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hirochachacha/go-smb2"
)

// The image store is where rips are written and where burns are read from.
// Two implementations: the local filesystem, and an SMB share ripperX
// talks to itself - no cifs mount, no root, no mount unit, just an address
// and a password in the settings file.
//
// It is deliberately a flat directory of files rather than a tree. A disc
// becomes one image, or one tar, with one name; that is what makes it
// possible to say "the file called this" in a URL without ever joining a
// user-supplied path onto a directory.

// validName reports whether name is a plain file name, safe to join onto a
// local directory or an SMB path.
//
// The checks that look sufficient are not: filepath.Base only splits on the
// separator of the machine ripperX runs on, so on Linux it happily passes
// `..\..\secret.iso` straight through to the backslash-separated path an SMB
// share uses. So both separators are refused by name, along with the two
// traversal names, the control characters, and the colon that would name an
// alternate data stream on a Windows server.
//
// What is deliberately allowed is everything else - spaces, accents, any
// script. This was an allowlist of ASCII letters and digits, which is right
// for names ripperX invents and wrong for a directory of files it did not:
// a shelf of installer images includes "tiny11 23H2 x64.iso", and refusing
// to list a file is not a security property, it is a file nobody can burn.
// Names ripperX creates still go through safeName and stay tidy.
func validName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	// "." and ".." are directories, and a leading dot is a hidden file that
	// has no business being offered as a disc image.
	if strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		switch {
		case r == '/', r == '\\':
			return false // a path separator on one system or the other
		case r == ':':
			return false // an alternate data stream on a Windows server
		case r < 0x20, r == 0x7f:
			return false // control characters, including NUL
		}
	}
	return true
}

// safeName turns anything - a volume label with spaces and accents in it, a
// file name typed by a user - into a name validName accepts, without ever
// producing an empty one. It is applied to every name ripperX invents and
// to every name a client supplies.
//
// Letters and digits of any script are kept. A disc labelled in Cyrillic
// should rip to a file named in Cyrillic: the share stores names as UTF-16
// and the page sends them as UTF-8, so the only thing that ever lost them
// was this function insisting on ASCII. What is replaced is punctuation and
// whitespace, which is about what a file name can hold rather than about
// which alphabet it is in.
func safeName(name string, fallback string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r),
			r == '.', r == '_', r == '+':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			// A dash is kept, and counts as one: " - " between two words
			// is one separator however it was typed.
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// Runs of anything else collapse into a single dash, so
			// "Windows 98  SE" does not become "Windows-98--SE".
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	// Cut on a rune boundary: half of a two-byte letter is not a name.
	for len(out) > 180 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	out = strings.Trim(out, "-.")
	if !validName(out) {
		return fallback
	}
	return out
}

// storedFile is one image in the store.
type storedFile struct {
	Name    string    `json:"name" doc:"the file's name, which is how it is addressed"`
	Size    int64     `json:"size" doc:"size in bytes"`
	ModTime time.Time `json:"modTime" doc:"when it was last written"`
}

type store interface {
	Create(name string) (io.WriteCloser, error)
	Open(name string) (io.ReadSeekCloser, storedFile, error)
	List() ([]storedFile, error)
	Remove(name string) error
	// Describe returns the location to show in the UI. It never contains a
	// password.
	Describe() string
	Kind() string
	// Space reports what is left where images are written and how much
	// there is in all. ok is false where that cannot be established, which
	// is a read-only library and a share whose server will not say.
	Space() (free, total int64, ok bool)
}

// openStore picks where images live: an SMB share when one is configured,
// the local directory otherwise. A share is proven reachable here, so a
// wrong address or password fails at startup rather than after a rip that
// took twenty minutes.
func openStore(smbAddress, smbUser, smbPassword, smbDomain, dir string) (store, error) {
	if smbAddress == "" {
		return newLocalStore(dir)
	}
	st, err := newSMBStore(smbAddress, smbUser, smbPassword, smbDomain)
	if err != nil {
		return nil, err
	}
	if err := st.check(); err != nil {
		return nil, err
	}
	return st, nil
}

// errBadName is returned rather than a path error, so nothing about the
// layout of the store leaks back to whoever asked for the odd name.
var errBadName = errors.New("not a valid file name")

var errNoSuchImage = errors.New("no image by that name")

type localStore struct{ dir string }

func newLocalStore(dir string) (*localStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot use the image directory %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &localStore{dir: abs}, nil
}

func (l *localStore) Create(name string) (io.WriteCloser, error) {
	if !validName(name) {
		return nil, errBadName
	}
	return os.Create(filepath.Join(l.dir, name))
}

func (l *localStore) Open(name string) (io.ReadSeekCloser, storedFile, error) {
	if !validName(name) {
		return nil, storedFile{}, errBadName
	}
	f, err := os.Open(filepath.Join(l.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, storedFile{}, errNoSuchImage
	}
	if err != nil {
		return nil, storedFile{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, storedFile{}, err
	}
	return f, storedFile{Name: name, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (l *localStore) List() ([]storedFile, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []storedFile
	for _, e := range entries {
		if e.IsDir() || !validName(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, storedFile{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	sortStored(out)
	return out, nil
}

func (l *localStore) Remove(name string) error {
	if !validName(name) {
		return errBadName
	}
	err := os.Remove(filepath.Join(l.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return errNoSuchImage
	}
	return err
}

func (l *localStore) Describe() string { return l.dir }
func (l *localStore) Kind() string     { return "local" }

// FreeBytes is what stops a rip from filling a root filesystem. The figure
// is the space available to this user, not the total free space, which is
// what actually matters on a filesystem with reserved blocks.
func (l *localStore) Space() (int64, int64, bool) { return diskSpace(l.dir) }

// localPath is the real path of a stored file, which the burner program
// needs because it is a separate process and cannot be handed a file
// handle. Only the local store has one; an image on a share is staged to a
// temporary file before it is burned.
func (l *localStore) localPath(name string) (string, bool) {
	if !validName(name) {
		return "", false
	}
	return filepath.Join(l.dir, name), true
}

func sortStored(f []storedFile) {
	sort.Slice(f, func(i, j int) bool {
		if !f[i].ModTime.Equal(f[j].ModTime) {
			return f[i].ModTime.After(f[j].ModTime)
		}
		return f[i].Name < f[j].Name
	})
}

// smbStore keeps images on an SMB share. ripperX speaks SMB itself rather
// than leaning on a cifs mount, so it needs no root, no cifs-utils and no
// mount unit: the share is configured in /etc/ripperx.conf and nowhere else.
//
// One connection per operation. A rip takes minutes and a listing is rare,
// so session setup costs nothing worth caching, and there is no idle
// session for the server to drop and for us to discover half-way through a
// write.
type smbStore struct {
	host   string // host:port
	share  string
	dir    string // subdirectory within the share; "" is the share root
	user   string
	pass   string
	domain string

	// What the server last said about the room left on the share, and when.
	spaceMu    sync.Mutex
	spaceAt    time.Time
	spaceFree  int64
	spaceTotal int64
	spaceOK    bool
}

// spaceCacheFor is how long the share's free space is believed. Long enough
// that listing the images and starting a rip do not ask twice; short enough
// that the figure on the page is the figure now.
const spaceCacheFor = 10 * time.Second

// newSMBStore parses //host[:port]/share[/subdir]. Backslashes and an smb://
// prefix are accepted too, since that is how the same address gets written
// in Windows and in a browser.
func newSMBStore(address, user, pass, domain string) (*smbStore, error) {
	a := strings.ReplaceAll(address, `\`, "/")
	a = strings.TrimPrefix(a, "smb:")
	parts := strings.Split(strings.Trim(a, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("smb-address %q: expected //host/share or //host/share/subdir", address)
	}
	if user == "" {
		return nil, errors.New("smb-address is set but smb-user is not")
	}
	host := parts[0]
	// A bare IPv6 literal would be ambiguous here; it has to be written in
	// brackets, as [::1]:445, which this test then leaves alone.
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "445")
	}
	return &smbStore{
		host:   host,
		share:  parts[1],
		dir:    strings.Join(parts[2:], `\`),
		user:   user,
		pass:   pass,
		domain: domain,
	}, nil
}

// smbSession is the three-layer stack a single operation needs, so it can
// be closed again as one thing.
type smbSession struct {
	conn  net.Conn
	sess  *smb2.Session
	share *smb2.Share
}

func (s *smbStore) connect() (*smbSession, error) {
	conn, err := net.DialTimeout("tcp", s.host, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("smb: connecting to %s: %w", s.host, err)
	}
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User: s.user, Password: s.pass, Domain: s.domain,
	}}
	sess, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smb: logging in to %s as %s: %w", s.host, s.user, err)
	}
	share, err := sess.Mount(s.share)
	if err != nil {
		sess.Logoff()
		conn.Close()
		return nil, fmt.Errorf("smb: mounting share %q: %w", s.share, err)
	}
	return &smbSession{conn: conn, sess: sess, share: share}, nil
}

// Close tears the stack down innermost first, keeping the first error.
// Logoff closes the TCP connection itself, so the Close here is only for
// the case where logoff failed before getting that far; an already-closed
// connection is the normal outcome, not an error.
func (c *smbSession) Close() error {
	err := c.share.Umount()
	if e := c.sess.Logoff(); err == nil {
		err = e
	}
	if e := c.conn.Close(); err == nil && !errors.Is(e, net.ErrClosed) {
		err = e
	}
	return err
}

// smbFile holds its session open for as long as the caller holds the file.
type smbFile struct {
	*smb2.File
	sess *smbSession
}

func (f *smbFile) Close() error {
	err := f.File.Close()
	if e := f.sess.Close(); err == nil {
		err = e
	}
	return err
}

func (s *smbStore) Create(name string) (io.WriteCloser, error) {
	if !validName(name) {
		return nil, errBadName
	}
	c, err := s.connect()
	if err != nil {
		return nil, err
	}
	if s.dir != "" {
		// A share whose subdirectory does not exist yet is a configuration
		// that should work, not one that fails on the first rip.
		_ = c.share.MkdirAll(strings.ReplaceAll(s.dir, `\`, "/"), 0o755)
	}
	f, err := c.share.Create(s.remotePath(name))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("smb: creating %s: %w", s.remotePath(name), err)
	}
	return &smbFile{File: f, sess: c}, nil
}

func (s *smbStore) Open(name string) (io.ReadSeekCloser, storedFile, error) {
	if !validName(name) {
		return nil, storedFile{}, errBadName
	}
	c, err := s.connect()
	if err != nil {
		return nil, storedFile{}, err
	}
	f, err := c.share.Open(s.remotePath(name))
	if err != nil {
		c.Close()
		if os.IsNotExist(err) {
			return nil, storedFile{}, errNoSuchImage
		}
		return nil, storedFile{}, fmt.Errorf("smb: opening %s: %w", s.remotePath(name), err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		c.Close()
		return nil, storedFile{}, err
	}
	return &smbFile{File: f, sess: c}, storedFile{Name: name, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (s *smbStore) List() ([]storedFile, error) {
	c, err := s.connect()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dir := strings.ReplaceAll(s.dir, `\`, "/")
	if dir == "" {
		dir = "."
	}
	entries, err := c.share.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // a share whose subdirectory has yet to be created
		}
		return nil, fmt.Errorf("smb: listing %s: %w", s.Describe(), err)
	}
	var out []storedFile
	for _, e := range entries {
		if e.IsDir() || !validName(e.Name()) {
			continue
		}
		out = append(out, storedFile{Name: e.Name(), Size: e.Size(), ModTime: e.ModTime()})
	}
	sortStored(out)
	return out, nil
}

func (s *smbStore) Remove(name string) error {
	if !validName(name) {
		return errBadName
	}
	c, err := s.connect()
	if err != nil {
		return err
	}
	defer c.Close()
	err = c.share.Remove(s.remotePath(name))
	if os.IsNotExist(err) {
		return errNoSuchImage
	}
	return err
}

// check proves the address and credentials work, so a bad configuration is
// a startup failure rather than a rip that runs for minutes and then cannot
// be saved.
func (s *smbStore) check() error {
	c, err := s.connect()
	if err != nil {
		return err
	}
	return c.Close()
}

func (s *smbStore) remotePath(name string) string {
	if s.dir == "" {
		return name
	}
	return s.dir + `\` + name
}

func (s *smbStore) Describe() string {
	host := strings.TrimSuffix(s.host, ":445")
	out := "//" + host + "/" + s.share
	if s.dir != "" {
		out += "/" + strings.ReplaceAll(s.dir, `\`, "/")
	}
	return out
}

func (s *smbStore) Kind() string { return "smb" }

// Space asks the server how much room is left on the share.
//
// It is worth the round trip twice over. Filling a NAS is as easy as filling
// a disk, and a rip that runs out of room does so twenty minutes in, on a
// disc that then has to be read again. The figure is cached for a few
// seconds because two things ask for it at once - the page listing the
// images, and the check in front of every rip.
func (s *smbStore) Space() (int64, int64, bool) {
	s.spaceMu.Lock()
	defer s.spaceMu.Unlock()
	if time.Since(s.spaceAt) < spaceCacheFor {
		return s.spaceFree, s.spaceTotal, s.spaceOK
	}
	s.spaceAt = time.Now()
	s.spaceFree, s.spaceTotal, s.spaceOK = 0, 0, false

	c, err := s.connect()
	if err != nil {
		return 0, 0, false
	}
	defer c.Close()
	dir := strings.ReplaceAll(s.dir, `\`, "/")
	if dir == "" {
		dir = "."
	}
	st, err := c.share.Statfs(dir)
	if err != nil {
		// A share whose folder does not exist yet, or a server that will
		// not answer: not knowing is a normal answer, not a failure.
		return 0, 0, false
	}
	unit := allocationUnit(st.BlockSize(), st.FragmentSize())
	if unit <= 0 {
		return 0, 0, false
	}
	// AvailableBlockCount rather than FreeBlockCount: on a share with a
	// quota those differ, and the quota is what applies here.
	s.spaceFree = int64(st.AvailableBlockCount()) * unit
	s.spaceTotal = int64(st.TotalBlockCount()) * unit
	s.spaceOK = s.spaceTotal > 0
	return s.spaceFree, s.spaceTotal, s.spaceOK
}

// allocationUnit is how many bytes one of the counts SMB returns stands
// for.
//
// The counts are in allocation units, and an allocation unit is
// SectorsPerAllocationUnit * BytesPerSector. The library's names for those
// two are worth reading twice: BlockSize is the bytes in a sector, and
// FragmentSize is the number of sectors in an allocation unit - a count,
// not a size. Multiplying by BlockSize alone reports a share as exactly
// SectorsPerAllocationUnit times smaller than it is, which on a 7.5 TB
// share with two sectors to the unit is a confident, wrong 3.8 TB.
func allocationUnit(bytesPerSector, sectorsPerUnit uint64) int64 {
	if bytesPerSector == 0 {
		return 0
	}
	if sectorsPerUnit == 0 {
		// A server that does not say is taken at one sector to the unit
		// rather than not answered at all.
		sectorsPerUnit = 1
	}
	return int64(bytesPerSector) * int64(sectorsPerUnit)
}

// readOnlyStore is a store that refuses to be written to. It wraps the ISO
// library: a directory of installer images that ripperX burns from and must
// never put anything into or take anything out of.
//
// The refusal is structural rather than a convention, because a convention
// is one forgotten call away from deleting somebody's Windows ISO.
type readOnlyStore struct{ store }

var errReadOnlyStore = errors.New("this is a read-only library; nothing can be written to it")

func (readOnlyStore) Create(string) (io.WriteCloser, error) { return nil, errReadOnlyStore }
func (readOnlyStore) Remove(string) error                   { return errReadOnlyStore }

// Space is meaningless for a library nothing is written to.
func (readOnlyStore) Space() (int64, int64, bool) { return 0, 0, false }

// localPath is deliberately not forwarded: the burner is handed a path only
// through stageImage, which asks the writable store. Forwarding it would
// let a caller reach the underlying directory and write there.

// openReadOnlyStore opens the ISO library. The address is an SMB path when
// it looks like one and a local directory otherwise, so the same setting
// serves a share and a folder.
func openReadOnlyStore(address, user, password, domain string) (store, error) {
	if address == "" {
		return nil, nil
	}
	if !looksLikeSMB(address) {
		local, err := newLocalStore(address)
		if err != nil {
			return nil, err
		}
		return readOnlyStore{local}, nil
	}
	st, err := newSMBStore(address, user, password, domain)
	if err != nil {
		return nil, err
	}
	if err := st.check(); err != nil {
		return nil, err
	}
	return readOnlyStore{st}, nil
}

func looksLikeSMB(address string) bool {
	return strings.HasPrefix(address, "//") ||
		strings.HasPrefix(address, `\\`) ||
		strings.HasPrefix(address, "smb:")
}

// seekReaderAt presents a seekable file as a ReaderAt, which is what the
// filesystem and boot readers take. It is not safe for concurrent use -
// there is one file position and it moves - so it is for one caller
// reading one image at a time, which is what inspecting an image is.
type seekReaderAt struct{ rs io.ReadSeeker }

func (s seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s.rs, p)
	// ReadAt's contract is io.EOF for a short read; ReadFull says
	// ErrUnexpectedEOF for the same thing.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
