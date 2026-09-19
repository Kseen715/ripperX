package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Kseen715/ripperX/mmc"
	_ "modernc.org/sqlite"
)

// A surface scan is only worth doing twice. One scan says what a disc is
// like today; two, a year apart, say which way it is going - and that is
// the thing you actually want to know about a shelf of discs. So scans are
// written to a database rather than kept in memory, and a disc is
// recognised when it comes back by a fingerprint taken from its table of
// contents and its volume, not by whatever it happens to be called.
//
// Finished jobs go in the same file. They were in memory only, and a
// restart lost the record of what had been ripped from what; there is no
// reason for that once there is a database to put them in.
//
// SQLite through modernc.org/sqlite, which is Go rather than a binding, so
// the binary stays static and cross-compiles as it did before.

type history struct {
	db   *sql.DB
	path string
}

// schema is applied on open and is idempotent. The version pragma is there
// so a later change can be migrated rather than guessed at.
const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    drive       TEXT,
    label       TEXT,
    state       TEXT NOT NULL,
    started     INTEGER NOT NULL,   -- unix milliseconds
    finished    INTEGER,
    done        INTEGER NOT NULL DEFAULT 0,
    total       INTEGER NOT NULL DEFAULT 0,
    bad_sectors INTEGER NOT NULL DEFAULT 0,
    bad_ranges  TEXT,               -- JSON array
    sha256      TEXT,
    targets     TEXT,               -- JSON array
    message     TEXT,
    error       TEXT
);
CREATE INDEX IF NOT EXISTS jobs_started ON jobs(started DESC);

-- One row per scan. disc is the fingerprint, so the scans of one disc over
-- the years are a single query.
CREATE TABLE IF NOT EXISTS scans (
    job_id       TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    disc         TEXT NOT NULL,
    label        TEXT,
    profile      TEXT,
    drive        TEXT,
    scanned_at   INTEGER NOT NULL,
    sectors      INTEGER NOT NULL,
    c2_supported INTEGER NOT NULL,
    c2_total     INTEGER NOT NULL,
    c2_sectors   INTEGER NOT NULL,
    c2_max       INTEGER NOT NULL,
    unreadable   INTEGER NOT NULL,
    slow_blocks  INTEGER NOT NULL,
    read_seconds REAL    NOT NULL,
    avg_kbps     REAL    NOT NULL,
    min_kbps     REAL    NOT NULL,
    grade        TEXT    NOT NULL,
    score        REAL    NOT NULL,
    summary      TEXT,
    map          TEXT               -- JSON array of per-bucket severity
);
CREATE INDEX IF NOT EXISTS scans_disc ON scans(disc, scanned_at DESC);
CREATE INDEX IF NOT EXISTS scans_at ON scans(scanned_at DESC);
`

// openHistory opens or creates the database. A database that cannot be
// opened is reported but is not fatal: ripperX drives hardware, and
// refusing to rip a disc because a history file is on a read-only
// filesystem would be the wrong trade.
func openHistory(path string) (*history, error) {
	// WAL keeps a scan writing its result from blocking the page reading
	// the list, and busy_timeout covers the moment where they still meet.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening the history database %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("preparing the history database: %w", err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return &history{db: db, path: path}, nil
}

func (h *history) close() error {
	if h == nil {
		return nil
	}
	return h.db.Close()
}

// defaultHistoryPath puts the database beside the images when they are
// local, because that is the directory a user already knows about and
// already backs up. With the images on a share it goes in the working
// directory instead: a SQLite file on SMB is a good way to corrupt one.
func defaultHistoryPath(st store, out string) string {
	if local, ok := st.(*localStore); ok {
		return filepath.Join(local.dir, "ripperx.db")
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "ripperx.db"
	}
	return filepath.Join(abs, "ripperx.db")
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func asJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// saveJob records a finished job. It is called for every kind, so the
// database answers "what did I rip off that disc, and when" as well as the
// scan questions.
func (h *history) saveJob(j Job) error {
	if h == nil {
		return nil
	}
	_, err := h.db.Exec(`
        INSERT INTO jobs (id, kind, drive, label, state, started, finished,
                          done, total, bad_sectors, bad_ranges, sha256,
                          targets, message, error)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET
            state=excluded.state, finished=excluded.finished, done=excluded.done,
            total=excluded.total, bad_sectors=excluded.bad_sectors,
            bad_ranges=excluded.bad_ranges, sha256=excluded.sha256,
            targets=excluded.targets, message=excluded.message, error=excluded.error`,
		j.ID, j.Kind, j.Drive, j.Label, string(j.State), millis(j.Started), millis(j.Finished),
		j.Done, j.Total, j.BadSectors, asJSON(j.BadRanges), j.SHA256,
		asJSON(j.Targets), j.Message, j.Error)
	return err
}

func (h *history) saveScan(j Job, fingerprint, label string, disc *mmc.Disc, res *ScanResult) error {
	if h == nil {
		return nil
	}
	// The job row has to exist first: the scan references it.
	if err := h.saveJob(j); err != nil {
		return err
	}
	_, err := h.db.Exec(`
        INSERT INTO scans (job_id, disc, label, profile, drive, scanned_at, sectors,
                           c2_supported, c2_total, c2_sectors, c2_max, unreadable,
                           slow_blocks, read_seconds, avg_kbps, min_kbps,
                           grade, score, summary, map)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(job_id) DO NOTHING`,
		j.ID, fingerprint, label, disc.ProfileName, j.Drive, millis(time.Now()), res.Sectors,
		boolToInt(res.C2Supported), res.C2Total, res.C2Sectors, res.C2Max, res.Unreadable,
		res.SlowBlocks, res.ReadSeconds, res.AvgKBps, res.MinKBps,
		string(res.Grade), res.Score, res.Summary, asJSON(res.Map))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pastJob is a finished job read back out of the database.
type pastJob struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Drive      string    `json:"drive,omitempty"`
	Label      string    `json:"label"`
	State      string    `json:"state"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished,omitzero"`
	Done       int64     `json:"done"`
	Total      int64     `json:"total"`
	BadSectors int64     `json:"badSectors"`
	SHA256     string    `json:"sha256,omitempty"`
	Targets    []string  `json:"targets,omitempty"`
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (h *history) recentJobs(limit int) ([]pastJob, error) {
	if h == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := h.db.Query(`
        SELECT id, kind, COALESCE(drive,''), COALESCE(label,''), state, started,
               COALESCE(finished,0), done, total, bad_sectors, COALESCE(sha256,''),
               COALESCE(targets,'[]'), COALESCE(message,''), COALESCE(error,'')
        FROM jobs ORDER BY started DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []pastJob{}
	for rows.Next() {
		var j pastJob
		var started, finished int64
		var targets string
		if err := rows.Scan(&j.ID, &j.Kind, &j.Drive, &j.Label, &j.State, &started,
			&finished, &j.Done, &j.Total, &j.BadSectors, &j.SHA256,
			&targets, &j.Message, &j.Error); err != nil {
			return nil, err
		}
		j.Started, j.Finished = fromMillis(started), fromMillis(finished)
		_ = json.Unmarshal([]byte(targets), &j.Targets)
		out = append(out, j)
	}
	return out, rows.Err()
}

// scanRecord is one past scan.
type scanRecord struct {
	JobID     string    `json:"jobId"`
	Disc      string    `json:"disc" doc:"the disc's fingerprint; the same disc scans to the same value"`
	Label     string    `json:"label"`
	Profile   string    `json:"profile"`
	Drive     string    `json:"drive"`
	ScannedAt time.Time `json:"scannedAt"`
	Sectors   int64     `json:"sectors"`
	C2Sectors int64     `json:"c2Sectors"`
	C2Total   int64     `json:"c2Total"`
	// Measured says the error rate could be read at all. A DVD's cannot,
	// and a trend that does not know the difference reports "not one
	// uncorrected byte" about a disc nothing was ever counted on.
	Measured   bool    `json:"measured"`
	Unreadable int64   `json:"unreadable"`
	Grade      Grade   `json:"grade"`
	Score      float64 `json:"score"`
	Summary    string  `json:"summary"`
	Map        []int   `json:"map,omitempty"`
}

// discHistory is one disc and every time it has been checked, oldest first,
// so the trend reads left to right.
type discHistory struct {
	Disc    string       `json:"disc"`
	Label   string       `json:"label"`
	Profile string       `json:"profile"`
	Scans   []scanRecord `json:"scans"`

	// Trend is what two or more scans add up to: better, worse, or the
	// same. It is the answer the database exists to give.
	Trend       string  `json:"trend" doc:"first, steady, worse or better"`
	TrendNote   string  `json:"trendNote" doc:"what changed since the previous scan, in words"`
	LatestGrade Grade   `json:"latestGrade"`
	LatestScore float64 `json:"latestScore"`
}

// discs returns every disc that has ever been scanned, most recently
// checked first. withMap is false for the list, because two hundred and
// forty numbers per scan is a lot to send for a summary.
func (h *history) discs(fingerprint string, withMap bool) ([]discHistory, error) {
	if h == nil {
		return nil, nil
	}
	query := `
        SELECT job_id, disc, COALESCE(label,''), COALESCE(profile,''), COALESCE(drive,''),
               scanned_at, sectors, c2_sectors, c2_total, unreadable, grade, score,
               c2_supported, COALESCE(summary,''), COALESCE(map,'[]')
        FROM scans`
	args := []any{}
	if fingerprint != "" {
		query += " WHERE disc = ?"
		args = append(args, fingerprint)
	}
	query += " ORDER BY scanned_at ASC"

	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDisc := map[string]*discHistory{}
	var order []string
	for rows.Next() {
		var r scanRecord
		var at int64
		var mapJSON string
		if err := rows.Scan(&r.JobID, &r.Disc, &r.Label, &r.Profile, &r.Drive,
			&at, &r.Sectors, &r.C2Sectors, &r.C2Total, &r.Unreadable,
			&r.Grade, &r.Score, &r.Measured, &r.Summary, &mapJSON); err != nil {
			return nil, err
		}
		r.ScannedAt = fromMillis(at)
		if withMap {
			_ = json.Unmarshal([]byte(mapJSON), &r.Map)
		}
		d, ok := byDisc[r.Disc]
		if !ok {
			d = &discHistory{Disc: r.Disc, Label: r.Label, Profile: r.Profile}
			byDisc[r.Disc] = d
			order = append(order, r.Disc)
		}
		// The most recent label wins: a disc rescanned after its volume was
		// read properly should show the better name.
		if r.Label != "" {
			d.Label = r.Label
		}
		d.Scans = append(d.Scans, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]discHistory, 0, len(order))
	for _, id := range order {
		d := byDisc[id]
		describeTrend(d)
		out = append(out, *d)
	}
	// Most recently checked first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// describeTrend compares the last two scans of a disc. The comparison is on
// the error rate rather than the score, because the score is bounded and
// two bad scans can both sit at its floor while the disc got twice as bad.
func describeTrend(d *discHistory) {
	if len(d.Scans) == 0 {
		return
	}
	last := d.Scans[len(d.Scans)-1]
	d.LatestGrade, d.LatestScore = last.Grade, last.Score
	if len(d.Scans) == 1 {
		d.Trend = "first"
		d.TrendNote = "Checked once. Check it again in a year to see which way it is going."
		return
	}
	prev := d.Scans[len(d.Scans)-2]
	gap := last.ScannedAt.Sub(prev.ScannedAt)

	rate := func(r scanRecord) float64 {
		if r.Sectors == 0 {
			return 0
		}
		return float64(r.C2Sectors) / float64(r.Sectors)
	}
	before, now := rate(prev), rate(last)
	newlyLost := last.Unreadable - prev.Unreadable

	switch {
	case newlyLost > 0:
		d.Trend = "worse"
		d.TrendNote = fmt.Sprintf(
			"%s became unreadable in the %s since the last check. Copy this disc now.",
			plural(newlyLost, "sector", "sectors"), roughly(gap))
	case !last.Measured || !prev.Measured:
		// Nothing was counted on at least one of them, so there is nothing
		// to compare. Saying the disc is unchanged would be claiming a
		// measurement that was never made.
		d.Trend = "steady"
		d.TrendNote = fmt.Sprintf(
			"Every sector still reads, %s on. The error rate cannot be measured on this disc, so whether it is decaying is not something these checks can tell you.",
			roughly(gap))
	case before == 0 && now == 0:
		d.Trend = "steady"
		d.TrendNote = fmt.Sprintf("Still not one uncorrected byte, %s on.", roughly(gap))
	case now > before*1.5 && now > 1e-5:
		d.Trend = "worse"
		d.TrendNote = fmt.Sprintf(
			"The error rate has gone from %.4f%% to %.4f%% in %s. This disc is decaying; copy it.",
			before*100, now*100, roughly(gap))
	case before > now*1.5 && before > 1e-5:
		// A disc does not heal. A better reading almost always means the
		// first scan was of a dirty disc, or a different drive read it.
		d.Trend = "better"
		d.TrendNote = fmt.Sprintf(
			"The error rate fell from %.4f%% to %.4f%%. Discs do not repair themselves - the earlier scan was most likely of a dirty disc, or a different drive read it.",
			before*100, now*100)
	default:
		d.Trend = "steady"
		d.TrendNote = fmt.Sprintf("About the same as %s ago.", roughly(gap))
	}
}

// roughly says how long ago in the units a person would use.
func roughly(d time.Duration) string {
	switch {
	case d < time.Hour:
		return plural(int64(d.Minutes()), "minute", "minutes")
	case d < 48*time.Hour:
		return plural(int64(d.Hours()), "hour", "hours")
	case d < 60*24*time.Hour:
		return plural(int64(d.Hours()/24), "day", "days")
	case d < 730*24*time.Hour:
		return plural(int64(d.Hours()/24/30), "month", "months")
	default:
		return plural(int64(d.Hours()/24/365), "year", "years")
	}
}

type historyResponse struct {
	Jobs    []pastJob `json:"jobs" doc:"finished jobs, newest first, from the database rather than from memory"`
	Path    string    `json:"path" doc:"where the history is kept"`
	Enabled bool      `json:"enabled" doc:"false when no history database could be opened"`
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	resp := historyResponse{Jobs: []pastJob{}, Enabled: s.history != nil}
	if s.history == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Path = s.history.path
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.history.recentJobs(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp.Jobs = jobs
	writeJSON(w, http.StatusOK, resp)
}

type discsResponse struct {
	Discs   []discHistory `json:"discs" doc:"every disc that has been checked, most recently checked first"`
	Enabled bool          `json:"enabled" doc:"false when no history database could be opened"`
}

func (s *server) handleDiscs(w http.ResponseWriter, r *http.Request) {
	resp := discsResponse{Discs: []discHistory{}, Enabled: s.history != nil}
	if s.history == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// One disc asked for by fingerprint comes back with its severity maps,
	// because that is the view that draws them.
	fingerprint := r.URL.Query().Get("disc")
	discs, err := s.history.discs(fingerprint, fingerprint != "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if fingerprint != "" && len(discs) == 0 {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no disc by that fingerprint has been checked"})
		return
	}
	resp.Discs = discs
	writeJSON(w, http.StatusOK, resp)
}

var errNoHistory = errors.New("this server has no history database")
