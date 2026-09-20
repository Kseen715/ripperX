package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kseen715/ripperX/discfs"
	"github.com/Kseen715/ripperX/iso9660"
	"github.com/Kseen715/ripperX/mmc"
	"github.com/Kseen715/ripperX/udf"
)

// A drive is one piece of hardware, and everything that wants it has to
// take a turn. A rip holds it for minutes; a directory listing holds it for
// a fraction of a second. Both are exclusive of each other, because two
// readers interleaved on one laser mean two seeks per sector and a rip that
// takes an hour instead of five minutes.
//
// So the lock is a read/write lock with a name attached. A job takes it for
// writing and records what it is doing; a listing takes it for reading and,
// if a job holds it, is told which job rather than left waiting. That is
// what lets the page say "ripping, 40%" instead of hanging.

type drive struct {
	id   string
	path string

	// rw guards the device itself. Jobs take it exclusively, short reads
	// share it.
	rw sync.RWMutex

	// mu guards everything below, including busy - which has to be
	// readable without waiting on rw, since saying "busy" is the whole
	// point.
	mu   sync.Mutex
	busy string // job id holding the drive, or ""
	dev  *mmc.Drive
	caps *mmc.Capabilities
	disc *mmc.Disc
	// iso is the filesystem on the disc in the drive, whichever of the two
	// formats it turned out to be.
	iso    discfs.FS
	isoErr error
	// discAt is when disc was last read, so a stale answer can be refreshed
	// without asking the drive on every request.
	discAt time.Time

	// media is what has been worked out about the disc for a browser to
	// play: durations and encoded segments, both dropped with the disc.
	media *mediaCache
	// transcodes bounds how many encoders may read this drive at once.
	transcodes chan struct{}
}

type driveSet struct {
	mu    sync.Mutex
	order []string
	byID  map[string]*drive
}

func newDriveSet(paths []string) *driveSet {
	ds := &driveSet{byID: make(map[string]*drive, len(paths))}
	for _, p := range paths {
		id := driveID(p)
		if _, dup := ds.byID[id]; dup {
			continue
		}
		ds.byID[id] = &drive{
			id: id, path: p,
			media:      newMediaCache(),
			transcodes: make(chan struct{}, transcodeSlots),
		}
		ds.order = append(ds.order, id)
	}
	return ds
}

// driveID is the name a drive is known by in URLs: the device node's base
// name, which is already unique and already what a person would call it.
func driveID(path string) string {
	id := filepath.Base(path)
	if id == "" || id == "." || id == "/" {
		return strings.NewReplacer("/", "-", ".", "-").Replace(path)
	}
	return id
}

func (ds *driveSet) count() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.order)
}

func (ds *driveSet) list() []*drive {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]*drive, 0, len(ds.order))
	for _, id := range ds.order {
		out = append(out, ds.byID[id])
	}
	return out
}

var errNoSuchDrive = errors.New("no drive by that name on this server")

func (ds *driveSet) get(id string) (*drive, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.byID[id]
	if !ok {
		return nil, errNoSuchDrive
	}
	return d, nil
}

func (ds *driveSet) closeAll() {
	for _, d := range ds.list() {
		d.mu.Lock()
		if d.dev != nil {
			_ = d.dev.Close()
			d.dev = nil
		}
		d.mu.Unlock()
	}
}

// open returns the device, opening it the first time. The handle is kept
// open for the life of the process: opening it is the slow part, and a
// drive is perfectly happy to be held open with nothing in it.
func (d *drive) open() (*mmc.Drive, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev != nil {
		return d.dev, nil
	}
	dev, err := mmc.Open(d.path)
	if err != nil {
		return nil, err
	}
	d.dev = dev
	return dev, nil
}

// errDriveBusy names the job in the way, so the page can link to it.
type errDriveBusy struct{ job string }

func (e *errDriveBusy) Error() string {
	return fmt.Sprintf("this drive is in use by job %s", e.job)
}

// errDriveStreaming is a drive nobody owns but that is in the middle of
// sending a file to a browser. It is a different sentence from a drive held
// by a job, because what the user does about it is different: wait a moment,
// or cancel the download.
var errDriveStreaming = errors.New("this drive is streaming a file to a browser; stop that download first")

// reserve marks the drive taken before there is a job to name as the taker,
// and takes the device itself at the same moment. It is called while the
// request that asks for the job is still being answered, which is what lets
// a second rip be refused with 409 rather than becoming a job that fails a
// moment later.
//
// Taking the device here rather than in the job's goroutine is what keeps a
// rip started during a long download from appearing to start and then
// silently stalling: the lock is tried, not waited on, so the request is
// refused with a reason instead.
func (d *drive) reserve() error {
	deadline := time.Now().Add(reserveWait)
	for {
		d.mu.Lock()
		if d.busy != "" {
			held := d.busy
			d.mu.Unlock()
			return &errDriveBusy{job: held}
		}
		if d.rw.TryLock() {
			d.busy = pendingJob
			d.mu.Unlock()
			return nil
		}
		d.mu.Unlock()
		if time.Now().After(deadline) {
			return errDriveStreaming
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reserveWait is how long reserve will wait for the device before deciding
// that whoever has it is not going to give it back soon.
//
// It exists because the two kinds of reader are wildly different lengths. A
// status read holds the drive for milliseconds, and the event stream takes
// one twice a second for every page that is open - refusing a rip because
// one happened to be in flight would be absurd, and it would happen often.
// A download holds it for minutes, and that is what this is meant to
// refuse. Two seconds tells them apart with room to spare.
const reserveWait = 2 * time.Second

// pendingJob stands in for the job id between the reservation and the job
// actually starting. It never reaches a page: a request holding it is still
// being answered.
const pendingJob = "starting"

// unreserve undoes a reservation whose job never started, giving the device
// back.
func (d *drive) unreserve() {
	d.mu.Lock()
	if d.busy == pendingJob {
		d.busy = ""
		d.rw.Unlock()
	}
	d.mu.Unlock()
}

// hold names the job that has the drive. The device was taken by reserve,
// so this only writes down who to blame; every path out of the job's
// goroutine must still call release.
func (d *drive) hold(jobID string) {
	d.mu.Lock()
	d.busy = jobID
	d.mu.Unlock()
}

func (d *drive) release() {
	d.mu.Lock()
	held := d.busy != ""
	d.busy = ""
	d.mu.Unlock()
	if held {
		d.rw.Unlock()
	}
}

func (d *drive) heldBy() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy
}

// borrow runs fn with the drive held for reading: a listing, a stat, a
// stream. Several may run at once. It refuses while a job has the drive, so
// the caller gets an answer instead of a wait.
func (d *drive) borrow(fn func(*mmc.Drive) error) error {
	dev, err := d.open()
	if err != nil {
		return err
	}
	// TryRLock rather than RLock: a job holding the drive should produce an
	// answer, not a request that hangs for the length of a rip.
	if !d.rw.TryRLock() {
		if held := d.heldBy(); held != "" {
			return &errDriveBusy{job: held}
		}
		return errDriveStreaming
	}
	defer d.rw.RUnlock()
	return fn(dev)
}

// state re-reads the disc when the drive says it was swapped, or when the
// cached answer is older than maxAge. Everything the UI shows about a disc
// comes through here, so a disc changed in the tray is noticed without the
// page having to ask for a refresh.
func (d *drive) state(maxAge time.Duration) (*mmc.Capabilities, *mmc.Disc, error) {
	dev, err := d.open()
	if err != nil {
		return nil, nil, err
	}

	d.mu.Lock()
	caps, disc, at := d.caps, d.disc, d.discAt
	d.mu.Unlock()

	if held := d.heldBy(); held != "" {
		// A job has the drive. Answer from what was last known rather than
		// interrupting it, which is both faster and kinder to the rip.
		if caps != nil {
			return caps, disc, nil
		}
		return nil, nil, &errDriveBusy{job: held}
	}

	d.rw.RLock()
	defer d.rw.RUnlock()

	changed := false
	if disc != nil {
		if c, _, err := dev.MediaChanged(); err == nil {
			changed = c
		}
	}
	if caps == nil {
		c, err := dev.Capabilities()
		if err != nil {
			return nil, nil, err
		}
		caps = c
	}
	if disc == nil || changed || time.Since(at) > maxAge {
		newDisc, err := dev.ReadDisc()
		if err != nil {
			return caps, nil, err
		}
		disc = newDisc
		changed = true
	}

	d.mu.Lock()
	d.caps, d.disc, d.discAt = caps, disc, time.Now()
	if changed {
		// The filesystem belongs to the disc that was in the drive, not to
		// the drive; a swap invalidates it, and everything worked out about
		// what is on it with it.
		d.iso, d.isoErr = nil, nil
		d.media.clear()
	}
	d.mu.Unlock()
	return caps, disc, nil
}

// filesystem returns the filesystem on the disc, reading its descriptors
// the first time and keeping the result until the disc changes. A disc with
// no filesystem - an audio CD, a blank - gives an error that is remembered
// too, so a page that polls does not re-probe every time.
func (d *drive) filesystem() (discfs.FS, error) {
	caps, disc, err := d.state(5 * time.Second)
	if err != nil {
		return nil, err
	}
	_ = caps
	if disc == nil || !disc.Present {
		return nil, errors.New("there is no disc in this drive")
	}
	if disc.DataTracks == 0 {
		return nil, errors.New("this disc has no data track, so there is no filesystem to browse")
	}

	d.mu.Lock()
	cached, cachedErr := d.iso, d.isoErr
	d.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if cachedErr != nil {
		return nil, cachedErr
	}

	var fsys discfs.FS
	err = d.borrow(func(dev *mmc.Drive) error {
		var e error
		fsys, e = openDiscFS(dev.DataReaderAt(disc.Sectors), disc)
		return e
	})
	d.mu.Lock()
	d.iso, d.isoErr = fsys, err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return fsys, nil
}

// openDiscFS reads whichever filesystem is on the disc.
//
// ISO 9660 is tried first because it is the one a bridged disc wants read:
// a disc with both carries the same files under both, and the ISO 9660 side
// brings Joliet and Rock Ridge with it. UDF is tried when there is no ISO
// 9660 at all, which is the case for a DVD-Video, a game disc, and most
// DVDs written by Windows - discs that are full of files and that a reader
// of ISO 9660 alone calls empty.
func openDiscFS(r io.ReaderAt, disc *mmc.Disc) (discfs.FS, error) {
	fsys, isoErr := iso9660.OpenSession(r, disc.LastSessionStart)
	if isoErr == nil {
		return fsys, nil
	}
	ufs, udfErr := udf.Open(r, disc.Sectors)
	if udfErr == nil {
		return ufs, nil
	}
	// Neither: report the ISO 9660 failure, which is the one that describes
	// an ordinary disc, and name UDF so the answer is not misleading about
	// what was looked for.
	if errors.Is(udfErr, udf.ErrNotUDF) {
		return nil, fmt.Errorf("%w (and no UDF filesystem either)", isoErr)
	}
	return nil, fmt.Errorf("%w (its UDF filesystem could not be read: %v)", isoErr, udfErr)
}

// driveView is one drive as the page sees it: what it is, what it can do,
// what is in it, and whether anything is using it.
type driveView struct {
	ID   string `json:"id" doc:"the name this drive is addressed by, such as sr0"`
	Path string `json:"path" doc:"the device node"`
	Name string `json:"name" doc:"vendor and model, as the drive reports them"`

	Capabilities *mmc.Capabilities `json:"capabilities,omitempty" doc:"everything the drive says it can and cannot do"`
	Disc         *mmc.Disc         `json:"disc,omitempty" doc:"the disc in the drive now, if any"`
	Volume       *iso9660.Volume   `json:"volume,omitempty" doc:"the ISO 9660 volume on that disc, if it has one"`

	// What ripperX will offer for this disc, decided here so the page does
	// not have to repeat the reasoning.
	CanRipISO   bool   `json:"canRipIso" doc:"a 2048-byte-per-sector image can be read"`
	CanRipIMG   bool   `json:"canRipImg" doc:"raw 2352-byte sectors can be read, so an exact image is possible"`
	CanRipAudio bool   `json:"canRipAudio" doc:"the disc has audio tracks this drive can read"`
	CanBrowse   bool   `json:"canBrowse" doc:"the disc has a filesystem ripperX can read"`
	CanBurn     bool   `json:"canBurn" doc:"a whole disc can be burned in this drive right now"`
	BurnBlocker string `json:"burnBlocker,omitempty" doc:"why burning is not offered, when it is not"`
	// A disc that already has data on it usually cannot be burned and very
	// often can be added to. That is a different question and gets its own
	// answer; asking only about burning said "no" to both.
	CanAppend     bool   `json:"canAppend" doc:"files can be added to the disc in this drive as a further session"`
	AppendBlocker string `json:"appendBlocker,omitempty" doc:"why adding files is not offered, when it is not"`
	BrowseError   string `json:"browseError,omitempty" doc:"why the filesystem could not be read, when it could not"`
	// DVDTitles is how many titles the disc's own index names, and zero for
	// anything that is not a DVD-Video. The page offers watching on the
	// strength of it, because a DVD's files are not its films: only the
	// index says where one title ends and the next begins.
	DVDTitles int `json:"dvdTitles,omitempty" doc:"how many titles a DVD-Video names in its own index"`

	// Fingerprint identifies the disc itself rather than the drive it is
	// in: the same disc gives the same value whenever and wherever it is
	// read. It is what /api/discs is keyed by, so a page can show what the
	// last check of this disc found without having watched it happen. It is
	// only in the deep view, because working it out means reading the
	// volume.
	Fingerprint string `json:"fingerprint,omitempty" doc:"identifies the disc across drives and restarts; the key /api/discs uses"`

	Busy  string `json:"busy,omitempty" doc:"the id of the job holding this drive, if one is"`
	Error string `json:"error,omitempty" doc:"why the drive could not be asked, when it could not"`
}

func (s *server) viewDrive(d *drive, deep bool) driveView {
	v := driveView{ID: d.id, Path: d.path, Busy: d.heldBy()}
	caps, disc, err := d.state(5 * time.Second)
	if err != nil {
		var busy *errDriveBusy
		if !errors.As(err, &busy) {
			v.Error = err.Error()
		}
		return v
	}
	v.Capabilities, v.Disc = caps, disc
	if caps != nil {
		v.Name = caps.Info.String()
	}
	if disc == nil || !disc.Present {
		v.BurnBlocker = s.burnBlocker(caps, disc)
		v.CanBurn = v.BurnBlocker == ""
		v.AppendBlocker = s.appendBlocker(caps, disc)
		v.CanAppend = v.AppendBlocker == ""
		return v
	}

	v.CanRipISO = disc.DataTracks > 0
	v.CanRipIMG = disc.RawReadable && disc.Profile.IsCD()
	v.CanRipAudio = disc.AudioTracks > 0 && disc.RawReadable
	v.BurnBlocker = s.burnBlocker(caps, disc)
	v.CanBurn = v.BurnBlocker == ""
	v.AppendBlocker = s.appendBlocker(caps, disc)
	v.CanAppend = v.AppendBlocker == ""

	// Reading the volume descriptors costs a seek, so it is only done when
	// the caller asked for the deep answer - the drive page, not the list.
	if deep {
		v.Fingerprint, _ = s.discIdentity(d, disc)
	}
	if deep && disc.DataTracks > 0 {
		if fsys, err := d.filesystem(); err != nil {
			v.BrowseError = err.Error()
		} else {
			vol := fsys.Volume()
			v.Volume, v.CanBrowse = &vol, true
			// Reading the index is a handful of kilobytes and happens once
			// per disc, so the deep view is the right place to find out
			// whether there is anything to watch.
			if dvd, err := d.dvd(); err == nil {
				v.DVDTitles = len(dvd.Titles)
			}
		}
	} else if disc.DataTracks > 0 {
		v.CanBrowse = true
	}
	return v
}

// burnBlocker says, in one sentence, why this drive and this disc cannot be
// burned - or returns empty when they can. Having one function decide means
// the page, the API and the burn job itself all agree.
func (s *server) burnBlocker(caps *mmc.Capabilities, disc *mmc.Disc) string {
	if !s.allowBurn {
		return "this server was started with burning turned off"
	}
	if s.burner == nil {
		return "no burner program is installed; ripperX needs xorriso, cdrecord or wodim to write a disc"
	}
	if caps == nil {
		return "the drive has not said what it can do"
	}
	if !caps.Write.CDR && !caps.Write.CDRW && !caps.Write.DVDR && !caps.Write.DVDPlusR &&
		!caps.Write.DVDRW && !caps.Write.DVDPlusRW && !caps.Write.DVDRAM {
		return "this drive can only read"
	}
	if disc == nil || !disc.Present {
		return "there is no disc in the drive"
	}
	if !disc.Profile.IsWritable() {
		return fmt.Sprintf("a %s cannot be written to", disc.ProfileName)
	}
	switch disc.Status {
	case mmc.DiscComplete:
		if disc.Erasable {
			return "this disc is closed; erase it first"
		}
		return "this disc is closed and cannot be erased"
	case mmc.DiscAppendable:
		if disc.Erasable {
			return "this disc already has data on it; erase it first, or add to it instead"
		}
		if disc.Appendable {
			return "this disc already has data on it and cannot be erased - but it is not closed, so files can be added to it"
		}
		return "this disc already has data on it and cannot be erased"
	}
	return ""
}

// handleDrives lists every drive, shallowly: enough to draw the list
// without paying for a volume descriptor read per drive.
func (s *server) handleDrives(w http.ResponseWriter, r *http.Request) {
	drives := s.drives.list()
	out := make([]driveView, 0, len(drives))
	for _, d := range drives {
		out = append(out, s.viewDrive(d, false))
	}
	writeJSON(w, http.StatusOK, drivesResponse{Drives: out})
}

type drivesResponse struct {
	Drives []driveView `json:"drives" doc:"every drive this server offers"`
}

// driveParam pulls the drive out of the {id} in the request's pattern,
// which is how every per-drive endpoint is addressed.
func (s *server) driveParam(r *http.Request) (*drive, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errors.New("no drive named in the path")
	}
	return s.drives.get(id)
}

func (s *server) handleDrive(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.viewDrive(d, true))
}

// handleRefresh forgets what was known about the disc and asks again. It is
// what the page's refresh button does, for the case where a disc was
// swapped in a drive whose firmware did not mention it.
func (s *server) handleDriveRefresh(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	if held := d.heldBy(); held != "" {
		writeJSON(w, http.StatusConflict, errorResponse{Error: (&errDriveBusy{job: held}).Error()})
		return
	}
	d.mu.Lock()
	d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, s.viewDrive(d, true))
}

// handleTray opens or closes a tray. Ejecting is the one command here that
// touches the world outside the machine, so a server that does not want
// that can turn it off.
func (s *server) handleTray(w http.ResponseWriter, r *http.Request) {
	d, err := s.driveParam(r)
	action := r.PathValue("action")
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	if !s.allowEject {
		writeJSON(w, http.StatusForbidden,
			errorResponse{Error: "this server was started with tray control turned off"})
		return
	}
	if held := d.heldBy(); held != "" {
		writeJSON(w, http.StatusConflict, errorResponse{Error: (&errDriveBusy{job: held}).Error()})
		return
	}
	err = d.borrow(func(dev *mmc.Drive) error {
		switch action {
		case "eject":
			return dev.Eject()
		case "load":
			return dev.LoadTray()
		default:
			return fmt.Errorf("no such tray action %q", action)
		}
	})
	if err != nil {
		var busy *errDriveBusy
		switch {
		case errors.As(err, &busy):
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		case strings.HasPrefix(err.Error(), "no such tray action"):
			writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	// Whatever was known about the disc is now wrong.
	d.mu.Lock()
	d.disc, d.iso, d.isoErr, d.discAt = nil, nil, nil, time.Time{}
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, statusMessage{Status: "tray " + action + "ed"})
}

// What has been worked out about the disc in a drive, rather than about the
// drive: how long a film on it is, and the pieces of it already encoded for
// a browser. Both belong to the disc, so both are dropped the moment one is
// swapped - the same event that drops the filesystem.
//
// The segments are kept because seeking is the point of them. A player
// scrubbing backwards over ground it has covered asks for segments it has
// already been given, and answering those from memory is the difference
// between a scrub bar and another trip across the disc.
type mediaCache struct {
	mu        sync.Mutex
	durations map[string]float64
	segments  map[string][]byte
	order     []string
	bytes     int64
	limit     int64
	// dvd is the disc's own index of its titles, read once: a DVD keeps
	// several films in one set of files and only the index says where one
	// ends and the next begins.
	dvd    *dvdDisc
	dvdErr error
	// inflight is the segment each encoder is working on, so that asking
	// for a segment twice waits once.
	inflight map[string]*segmentJob
}

// segmentJob is one segment being encoded, and whoever is waiting for it.
type segmentJob struct {
	done chan struct{}
	seg  []byte
	err  error
}

// mediaCacheLimit is what a drive may keep. A segment of DVD video is one
// to two megabytes, so this is a few minutes of film either side of where
// the viewer is.
const mediaCacheLimit = 64 << 20

func newMediaCache() *mediaCache {
	return &mediaCache{
		durations: map[string]float64{},
		segments:  map[string][]byte{},
		inflight:  map[string]*segmentJob{},
		limit:     mediaCacheLimit,
	}
}

func (m *mediaCache) duration(path string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.durations[path]
	return d, ok
}

func (m *mediaCache) setDuration(path string, d float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations[path] = d
}

func segmentKey(path string, n int) string {
	return path + "\x00" + strconv.Itoa(n)
}

func (m *mediaCache) segment(path string, n int) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seg, ok := m.segments[segmentKey(path, n)]
	return seg, ok
}

func (m *mediaCache) setSegment(path string, n int, seg []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := segmentKey(path, n)
	if _, dup := m.segments[key]; dup {
		return
	}
	m.segments[key] = seg
	m.order = append(m.order, key)
	m.bytes += int64(len(seg))
	// Oldest first: a film is watched forwards, so what was encoded longest
	// ago is what is least likely to be asked for again.
	for m.bytes > m.limit && len(m.order) > 1 {
		oldest := m.order[0]
		m.order = m.order[1:]
		m.bytes -= int64(len(m.segments[oldest]))
		delete(m.segments, oldest)
	}
}

// beginSegment hands back the job encoding this segment, and says whether
// the caller is the one who has to do it.
func (m *mediaCache) beginSegment(key string) (*segmentJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, running := m.inflight[key]; running {
		return job, false
	}
	job := &segmentJob{done: make(chan struct{})}
	m.inflight[key] = job
	return job, true
}

func (m *mediaCache) finishSegment(key string, seg []byte, err error) {
	m.mu.Lock()
	job := m.inflight[key]
	delete(m.inflight, key)
	m.mu.Unlock()
	if job == nil {
		return
	}
	job.seg, job.err = seg, err
	close(job.done)
}

func (m *mediaCache) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations = map[string]float64{}
	m.segments = map[string][]byte{}
	m.order = nil
	m.bytes = 0
	m.dvd, m.dvdErr = nil, nil
	// Whatever is being encoded belongs to the disc that has just left the
	// drive. Its waiters are told so rather than handed the next disc's
	// pictures.
	for key, job := range m.inflight {
		job.err = errors.New("the disc was changed while this was being converted")
		close(job.done)
		delete(m.inflight, key)
	}
}

// dvd is the disc's index of its titles, read the first time it is wanted
// and kept with everything else that belongs to this disc. A disc that is
// not a DVD-Video is remembered as such too, so a page that polls does not
// look for VIDEO_TS on an audio CD twice a second.
func (d *drive) dvd() (*dvdDisc, error) {
	fsys, err := d.filesystem()
	if err != nil {
		return nil, err
	}
	d.media.mu.Lock()
	cached, cachedErr := d.media.dvd, d.media.dvdErr
	d.media.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if cachedErr != nil {
		return nil, cachedErr
	}

	var disc *dvdDisc
	err = d.borrow(func(*mmc.Drive) error {
		var e error
		disc, e = readDVD(fsys)
		return e
	})
	if driveUnavailable(err) {
		// The drive being busy says nothing about the disc, so it is not
		// remembered as an answer about it.
		return nil, err
	}
	d.media.mu.Lock()
	d.media.dvd, d.media.dvdErr = disc, err
	d.media.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return disc, nil
}
