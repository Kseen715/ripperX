package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"

	"sync"
	"time"
)

// Everything that takes longer than a request is a job: ripping a disc,
// burning one, erasing one, verifying one, converting an image. A job is
// started by a POST that returns immediately with its id, and watched on
// /api/events, so a page can be closed and reopened - on another machine,
// even - without losing sight of a rip that takes twenty minutes.
//
// The running jobs live in memory; a finished one is also written to the
// history database, so a restart loses nothing but the record of a job that
// was still going - which is not something to pretend survived anyway.

type jobState string

const (
	jobRunning   jobState = "running"
	jobDone      jobState = "done"
	jobFailed    jobState = "failed"
	jobCancelled jobState = "cancelled"
)

// Job is one long-running piece of work, as the page sees it.
type Job struct {
	ID    string   `json:"id" doc:"the id this job is addressed by"`
	Kind  string   `json:"kind" doc:"iso, img, audio, files, scan, download, burn, erase or convert"`
	Drive string   `json:"drive,omitempty" doc:"the drive this job is using, if any"`
	Label string   `json:"label" doc:"one line saying what this job is, for a person"`
	State jobState `json:"state" doc:"running, done, failed or cancelled"`
	Phase string   `json:"phase,omitempty" doc:"which part of the job is happening now"`

	Done  int64 `json:"done" doc:"bytes finished so far"`
	Total int64 `json:"total" doc:"bytes expected in total, or 0 when not known in advance"`
	// BytesPerSec is smoothed over the last few seconds rather than
	// averaged from the start, so it reflects what the drive is doing now.
	BytesPerSec float64 `json:"bytesPerSec" doc:"current throughput"`
	// ETASeconds is worked out when the snapshot is taken rather than
	// stored, so it cannot go stale in a frame that sat in a buffer. It is
	// absent when there is nothing to base it on.
	ETASeconds float64 `json:"etaSeconds,omitempty" doc:"seconds left at the rate this job is going now"`

	// Target is the name in the image store this job is writing, and
	// Targets every name when it writes more than one.
	Target  string   `json:"target,omitempty" doc:"the file this job wrote, in the image store"`
	Targets []string `json:"targets,omitempty" doc:"every file this job wrote, when it wrote several"`

	// SHA256 is over exactly the bytes written, and is what a verify
	// compares against.
	SHA256 string `json:"sha256,omitempty" doc:"SHA-256 of what was written"`

	// BadSectors counts sectors the drive could not read, and BadRanges
	// says where they were - as sector ranges, because a scratch produces a
	// run of them and listing four thousand numbers helps nobody.
	BadSectors int64    `json:"badSectors" doc:"sectors the drive could not read; their contents are zeroes"`
	BadRanges  []string `json:"badRanges,omitempty" doc:"where the unreadable sectors were"`

	// Scan is filled in by a surface scan and by nothing else: what the
	// disc's condition turned out to be.
	Scan *ScanResult `json:"scan,omitempty" doc:"for a scan job, what it found"`

	Message  string    `json:"message,omitempty" doc:"the last thing worth saying about this job"`
	Error    string    `json:"error,omitempty" doc:"why the job failed, when it did"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
}

// Elapsed and ETA are derived rather than stored, so they cannot go stale
// in a snapshot that sat in a buffer.
func (j Job) elapsed() time.Duration {
	end := j.Finished
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.Started)
}

// eta estimates the seconds left. The recent rate is preferred, because a
// rip that has just hit a scratched patch should say so rather than promise
// the average it managed over the clean half; the average since the start
// is the fallback for the first seconds, before there is a recent rate.
//
// Zero means "no idea", which is honest for a job whose total is not known
// and better than a number pulled out of nothing.
func (j Job) eta() float64 {
	if j.State != jobRunning || j.Total <= 0 || j.Done <= 0 || j.Done >= j.Total {
		return 0
	}
	rate := j.BytesPerSec
	if rate <= 0 {
		if secs := j.elapsed().Seconds(); secs > 0 {
			rate = float64(j.Done) / secs
		}
	}
	if rate <= 0 {
		return 0
	}
	return float64(j.Total-j.Done) / rate
}

type jobRecord struct {
	mu     sync.Mutex
	job    Job
	cancel context.CancelFunc

	// Throughput is measured over a sliding pair of samples rather than
	// from the start: a rip that slows down on a scratched patch should say
	// so while it is happening.
	lastAt   time.Time
	lastDone int64

	// badFrom and badTo accumulate the current run of unreadable sectors,
	// flushed into BadRanges when the run ends.
	badFrom, badTo int64
	badOpen        bool
}

type jobManager struct {
	s *server

	mu    sync.Mutex
	byID  map[string]*jobRecord
	order []string

	hub *hub
}

// keptJobs is how many finished jobs are remembered. Enough to see what
// happened over an afternoon of ripping a shelf of discs, and bounded so a
// long-running server does not grow without limit.
const keptJobs = 60

func newJobManager(s *server) *jobManager {
	return &jobManager{s: s, byID: map[string]*jobRecord{}, hub: newHub()}
}

func newJobID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// A clock-based id is a fine fallback and this cannot fail twice.
		return fmt.Sprintf("j%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// start registers a job and runs fn in the background. fn is handed the
// record to report progress through, and a context that is cancelled when
// the job is cancelled or the server is shutting down.
func (m *jobManager) start(kind, driveID, label string, total int64, fn func(context.Context, *jobRecord) error) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &jobRecord{
		cancel: cancel,
		job: Job{
			ID:      newJobID(),
			Kind:    kind,
			Drive:   driveID,
			Label:   label,
			State:   jobRunning,
			Total:   total,
			Started: time.Now(),
		},
	}
	rec.lastAt = rec.job.Started

	m.mu.Lock()
	m.byID[rec.job.ID] = rec
	m.order = append(m.order, rec.job.ID)
	m.trimLocked()
	m.mu.Unlock()

	go func() {
		defer cancel()
		err := fn(ctx, rec)
		rec.finish(err, ctx.Err() != nil)
		m.broadcast()
		j := rec.snapshot()
		if herr := m.s.history.saveJob(j); herr != nil {
			log.Printf("job %s could not be filed in the history: %v", j.ID, herr)
		}
		if j.State == jobDone {
			log.Printf("job %s (%s) finished in %s", j.ID, j.Kind, j.elapsed().Round(time.Second))
		} else {
			log.Printf("job %s (%s) %s: %s", j.ID, j.Kind, j.State, j.Error)
		}
	}()

	m.broadcast()
	j := rec.snapshot()
	return &j
}

// startOnDrive is start for work that needs a drive to itself. The drive is
// reserved before the job exists, so a second request is refused while it is
// still a request - the caller turns the error into 409 - rather than
// becoming a job that fails a moment later. The job then holds the device
// for as long as it runs.
func (m *jobManager) startOnDrive(d *drive, kind, label string, total int64, fn func(context.Context, *jobRecord) error) (*Job, error) {
	if err := d.reserve(); err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			d.unreserve()
		}
	}()
	job := m.start(kind, d.id, label, total, func(ctx context.Context, rec *jobRecord) error {
		d.hold(rec.job.ID)
		defer d.release()
		m.broadcast()
		return fn(ctx, rec)
	})
	started = true
	return job, nil
}

// begin registers a job that this process does not run in a goroutine of
// its own - a download, which runs inside the request that asked for it.
// The caller must call end when the response is over, and should watch the
// returned context so that pressing Stop on the page actually stops it.
func (m *jobManager) begin(kind, driveID, label string, total int64) (*jobRecord, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &jobRecord{
		cancel: cancel,
		job: Job{
			ID: newJobID(), Kind: kind, Drive: driveID, Label: label,
			State: jobRunning, Total: total, Started: time.Now(),
		},
	}
	rec.lastAt = rec.job.Started

	m.mu.Lock()
	m.byID[rec.job.ID] = rec
	m.order = append(m.order, rec.job.ID)
	m.trimLocked()
	m.mu.Unlock()
	m.broadcast()
	return rec, ctx
}

// end closes a job begun with begin.
func (m *jobManager) end(rec *jobRecord, ctx context.Context, err error) {
	rec.finish(err, ctx.Err() != nil)
	rec.cancel()
	m.broadcast()
	if j := rec.snapshot(); j.State != jobDone {
		log.Printf("job %s (%s) %s: %s", j.ID, j.Kind, j.State, j.Error)
	}
}

// trimLocked drops the oldest finished jobs. A running job is never
// dropped, however old: it is the one thing the page must not lose.
func (m *jobManager) trimLocked() {
	if len(m.order) <= keptJobs {
		return
	}
	var keep []string
	drop := len(m.order) - keptJobs
	for _, id := range m.order {
		if drop > 0 && m.byID[id].snapshot().State != jobRunning {
			delete(m.byID, id)
			drop--
			continue
		}
		keep = append(keep, id)
	}
	m.order = keep
}

func (m *jobManager) get(id string) (*jobRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.byID[id]
	return rec, ok
}

// list returns every job, newest first, which is the order a page wants
// them in.
func (m *jobManager) list() []Job {
	m.mu.Lock()
	recs := make([]*jobRecord, 0, len(m.order))
	for _, id := range m.order {
		recs = append(recs, m.byID[id])
	}
	m.mu.Unlock()

	out := make([]Job, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

func (m *jobManager) cancelAll() {
	m.mu.Lock()
	recs := make([]*jobRecord, 0, len(m.order))
	for _, id := range m.order {
		recs = append(recs, m.byID[id])
	}
	m.mu.Unlock()
	for _, r := range recs {
		r.cancel()
	}
}

// broadcast pushes the whole picture - drives and jobs - to every watching
// page. A whole snapshot rather than a delta means a page that reconnects
// or missed a frame is correct again immediately.
func (m *jobManager) broadcast() {
	m.hub.publish(func() any { return m.s.snapshot() })
}

func (r *jobRecord) snapshot() Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.job
	j.BadRanges = append([]string(nil), r.job.BadRanges...)
	j.Targets = append([]string(nil), r.job.Targets...)
	j.ETASeconds = j.eta()
	return j
}

// progress records how far the job has got. It is called often - once per
// chunk of a rip - so it does no more than arithmetic; the pushing out to
// browsers is throttled by the hub.
func (r *jobRecord) progress(done int64) {
	r.mu.Lock()
	r.job.Done = done
	now := time.Now()
	if d := now.Sub(r.lastAt); d >= 500*time.Millisecond {
		rate := float64(done-r.lastDone) / d.Seconds()
		if r.job.BytesPerSec == 0 {
			r.job.BytesPerSec = rate
		} else {
			// A light exponential smoothing: enough to stop the figure
			// flickering, little enough that a real slowdown shows at once.
			r.job.BytesPerSec = 0.6*r.job.BytesPerSec + 0.4*rate
		}
		r.lastAt, r.lastDone = now, done
	}
	r.mu.Unlock()
}

func (r *jobRecord) setPhase(phase string) {
	r.mu.Lock()
	r.job.Phase = phase
	r.job.BytesPerSec = 0
	r.lastAt, r.lastDone = time.Now(), r.job.Done
	r.mu.Unlock()
}

func (r *jobRecord) setTotal(total int64) {
	r.mu.Lock()
	r.job.Total = total
	r.mu.Unlock()
}

func (r *jobRecord) say(format string, args ...any) {
	r.mu.Lock()
	r.job.Message = fmt.Sprintf(format, args...)
	r.mu.Unlock()
}

func (r *jobRecord) addTarget(name string) {
	r.mu.Lock()
	r.job.Targets = append(r.job.Targets, name)
	if r.job.Target == "" {
		r.job.Target = name
	}
	r.mu.Unlock()
}

// setScan records what the scan has found so far. The result is copied
// rather than pointed at: a scan updates its own copy on every block while
// the event hub is marshalling the last snapshot, and sharing the slice
// between the two is a data race that would show up as a torn frame or a
// crash under -race.
func (r *jobRecord) setScan(res *ScanResult) {
	c := *res
	c.Map = append([]int(nil), res.Map...)
	c.Notes = append([]string(nil), res.Notes...)
	c.BadRanges = append([]string(nil), res.BadRanges...)
	r.mu.Lock()
	r.job.Scan = &c
	r.mu.Unlock()
}

// flushBad closes the run of bad sectors that is still open, so BadRanges
// can be read before the job has finished. A scan needs it because the
// ranges go into its own result.
func (r *jobRecord) flushBad() {
	r.mu.Lock()
	r.flushBadLocked()
	r.mu.Unlock()
}

func (r *jobRecord) setHash(sum string) {
	r.mu.Lock()
	r.job.SHA256 = sum
	r.mu.Unlock()
}

// markBad records unreadable sectors, coalescing consecutive ones into a
// range. A scratch is thousands of sectors long and reporting it as one
// range is both smaller and more useful than reporting it as thousands.
func (r *jobRecord) markBad(sectors []int64) {
	if len(sectors) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range sectors {
		r.job.BadSectors++
		switch {
		case !r.badOpen:
			r.badFrom, r.badTo, r.badOpen = s, s, true
		case s == r.badTo+1:
			r.badTo = s
		default:
			r.flushBadLocked()
			r.badFrom, r.badTo, r.badOpen = s, s, true
		}
	}
}

func (r *jobRecord) flushBadLocked() {
	if !r.badOpen {
		return
	}
	// Only the first hundred ranges are kept: past that the disc is not
	// marginal, it is broken, and the count says so.
	if len(r.job.BadRanges) < 100 {
		if r.badFrom == r.badTo {
			r.job.BadRanges = append(r.job.BadRanges, fmt.Sprintf("%d", r.badFrom))
		} else {
			r.job.BadRanges = append(r.job.BadRanges, fmt.Sprintf("%d-%d", r.badFrom, r.badTo))
		}
	}
	r.badOpen = false
}

func (r *jobRecord) finish(err error, cancelled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushBadLocked()
	r.job.Finished = time.Now()
	r.job.BytesPerSec = 0
	r.job.Phase = ""
	switch {
	case cancelled && (err == nil || errors.Is(err, context.Canceled)):
		r.job.State = jobCancelled
		r.job.Error = "cancelled"
	case err != nil:
		r.job.State = jobFailed
		r.job.Error = err.Error()
	default:
		r.job.State = jobDone
	}
}

// snapshot is the whole shared picture: every drive and every job. It is
// what /api/state returns once and what /api/events pushes as things
// change.
type snapshot struct {
	Drives []driveView `json:"drives" doc:"every drive, shallowly - no volume descriptors are read"`
	Jobs   []Job       `json:"jobs" doc:"every job this server remembers, newest first"`
	Time   time.Time   `json:"time" doc:"when this snapshot was taken"`
}

func (s *server) snapshot() snapshot {
	drives := s.drives.list()
	views := make([]driveView, 0, len(drives))
	for _, d := range drives {
		views = append(views, s.viewDrive(d, false))
	}
	return snapshot{Drives: views, Jobs: s.jobs.list(), Time: time.Now()}
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jobsResponse{Jobs: s.jobs.list()})
}

type jobsResponse struct {
	Jobs []Job `json:"jobs" doc:"every job this server remembers, newest first"`
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no job by that id"})
		return
	}
	writeJSON(w, http.StatusOK, rec.snapshot())
}

func (s *server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no job by that id"})
		return
	}
	rec.cancel()
	writeJSON(w, http.StatusOK, statusMessage{Status: "cancelling"})
}
