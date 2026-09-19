package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Kseen715/ripperX/mmc"
)

// How healthy a disc is cannot be answered by reading it and seeing whether
// it worked. A disc reads perfectly right up until it does not: the error
// correction on a CD is strong enough to hide a great deal of damage, and
// the first outright read failure is the end of a decline that has been
// going on for years.
//
// What shows the decline is C2: for every byte of every sector the drive
// says whether the error correction had to give up on it. A disc with a
// rising C2 rate still reads today and is worth copying now. That is the
// measurement this makes, and it is the only portable one - it comes from
// the drive rather than from guessing at read speeds.
//
// Where C2 is not available - a DVD, or a drive that will not report it -
// the scan falls back to what can still be measured: which sectors are
// unreadable, and where the drive slowed down, which is the drive retrying
// internally and therefore damage by another name.

// Grade is the one word for a disc's condition. The thresholds behind each
// are in gradeDisc, and they are deliberately cautious: a disc called worn
// is one to copy this month, not one that has failed.
type Grade string

const (
	GradePristine Grade = "pristine" // not one uncorrected byte
	GradeGood     Grade = "good"     // a handful of flagged bytes; normal for a used disc
	GradeWorn     Grade = "worn"     // a real and rising error rate; copy it
	GradeDegraded Grade = "degraded" // heavy errors, still readable; copy it now
	GradeFailing  Grade = "failing"  // sectors the drive could not read at all
	GradeUnknown  Grade = "unknown"  // nothing could be measured
)

// ScanResult is what a surface scan learned. It is kept in the history
// database, so two scans of the same disc a year apart can be compared -
// which is the point of measuring rather than guessing.
type ScanResult struct {
	Sectors     int64 `json:"sectors" doc:"how many sectors were read"`
	C2Supported bool  `json:"c2Supported" doc:"whether the drive reported C2 error pointers for this disc"`

	// C2Total counts every byte the error correction could not fix;
	// C2Sectors how many sectors had at least one such byte, and C2Max the
	// worst single sector. The rate that matters is C2Sectors over Sectors.
	C2Total   int64 `json:"c2Total" doc:"bytes the drive could not correct, across the disc"`
	C2Sectors int64 `json:"c2Sectors" doc:"sectors with at least one uncorrected byte"`
	C2Max     int   `json:"c2Max" doc:"the worst single sector, in uncorrected bytes out of 2352"`

	Unreadable int64    `json:"unreadable" doc:"sectors the drive could not read at all"`
	BadRanges  []string `json:"badRanges,omitempty" doc:"where those sectors were"`
	// NoC2 counts sectors that read back perfectly but without their error
	// flags. Drives decline a C2 read within a few sectors of the lead-out,
	// where there is no read-ahead left; those sectors are fine, they are
	// simply not measured, and counting them as damage would condemn every
	// disc that was ever scanned.
	NoC2 int64 `json:"noC2" doc:"sectors that read back but whose error flags the drive would not give"`

	ReadSeconds float64 `json:"readSeconds" doc:"how long the scan took"`
	AvgKBps     float64 `json:"avgKbps" doc:"average read rate"`
	MinKBps     float64 `json:"minKbps" doc:"slowest block; a drive retrying reads slowly"`
	SlowBlocks  int     `json:"slowBlocks" doc:"blocks the drive read far slower than the track either side of them"`
	// SlowStretches is how many separate places those blocks are in, which
	// is the number worth telling a person: one scratch is several slow
	// blocks but it is still one scratch.
	SlowStretches int `json:"slowStretches" doc:"how many separate places the drive struggled"`

	Grade Grade   `json:"grade" doc:"pristine, good, worn, degraded, failing or unknown"`
	Score float64 `json:"score" doc:"0 to 100, from the error rate and the unreadable sectors"`

	// Map is the disc from the middle outwards, in fixed buckets, so a page
	// can draw a strip and show where the damage is. -1 is unreadable, -2 is
	// a stretch the drive had to fight to read; otherwise it is the worst C2
	// byte count in that bucket.
	//
	// The -2 matters most on a DVD, where there are no C2 pointers at all:
	// a drive slowing right down is retrying internally, and on a disc with
	// no other signal it is the only place the damage shows.
	Map []int `json:"map" doc:"severity per bucket; -1 unreadable, -2 the drive struggled, above 0 the uncorrected bytes"`
	// Buckets is how many of those buckets have been read so far. While a
	// scan is running the rest are not "clean", they are "not yet looked
	// at", and a strip that does not say so reads as a disc with a clean
	// bill of health it has not earned.
	Buckets int `json:"buckets" doc:"how many buckets of the map have been scanned; the rest are not yet read"`
	// Running says this result is a scan in progress rather than a verdict.
	Running bool `json:"running,omitempty" doc:"true while the scan is still going"`

	Summary string   `json:"summary" doc:"one sentence a person can act on"`
	Notes   []string `json:"notes,omitempty" doc:"what could not be measured, and why"`
}

// mapBuckets is how many columns the strip has. Enough to see that damage
// is at the outer edge - which is where a disc rots first, and what tells
// handling damage apart from manufacturing decay.
const mapBuckets = 240

// slowFactor is how far below its neighbours a block has to read before it
// counts as the drive struggling, and slowWindow is how many blocks either
// side "its neighbours" means.
//
// The comparison is local rather than against the whole disc because an
// optical drive spins at a constant angular velocity: the outside of a disc
// passes the laser far faster than the inside, so a read from start to
// finish speeds up by a factor of two or three all on its own. Measured
// against one figure for the whole disc, that ramp is indistinguishable
// from damage - and a 4 GB DVD, which spans the widest radius, gets called
// worn for being an ordinary DVD.
const (
	slowFactor = 3
	slowWindow = 16
	// slowRun is how many blocks in a row have to be slow before it is
	// reported. A scratch is a physical thing: it spans a contiguous piece
	// of track, and at 128 kB a block it covers several of them. One slow
	// block on its own is the host being busy, an interrupt, a cache miss -
	// noise that this heuristic produced three times before the rule went
	// in, and a real defect never once.
	slowRun = 3
	// spinUp is how long the drive is given to reach speed before its
	// timings mean anything. A scan starts with the disc stopped, and the
	// reads during the ramp come back below even 1x.
	spinUp = 3 * time.Second
)

type scanRequest struct {
	Drive   string `json:"drive" doc:"which drive, by its id"`
	SpeedKB int    `json:"speedKb,omitempty" doc:"read speed cap in kilobytes a second; a slower scan finds more"`
}

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	d, err := s.drives.get(req.Drive)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	_, disc, err := d.state(2 * time.Second)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if disc == nil || !disc.Present {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "there is no disc in this drive"})
		return
	}
	if disc.Sectors <= 0 {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "this disc has nothing on it to check"})
		return
	}

	speed := req.SpeedKB
	if speed == 0 {
		speed = s.readSpeedKB
	}
	fingerprint, label := s.discIdentity(d, disc)

	job, err := s.jobs.startOnDrive(d, "scan",
		fmt.Sprintf("checking the %s in %s", disc.ProfileName, d.id), disc.Sectors*mmc.SectorData,
		func(ctx context.Context, rec *jobRecord) error {
			res, err := s.scanDisc(ctx, rec, d, disc, speed)
			if err != nil {
				return err
			}
			rec.setScan(res)
			if s.history != nil {
				if err := s.history.saveScan(rec.snapshot(), fingerprint, label, disc, res); err != nil {
					rec.say("the scan finished but could not be filed: %v", err)
				}
			}
			return nil
		})
	if err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ripResponse{Job: *job})
}

// discIdentity returns a fingerprint that is the same every time this
// particular disc is put in a drive, and a label for a person. The
// fingerprint is what lets two scans a year apart be recognised as the same
// disc, which is the whole reason the history is kept.
func (s *server) discIdentity(d *drive, disc *mmc.Disc) (fingerprint, label string) {
	h := sha256.New()
	fmt.Fprintf(h, "profile=%d sectors=%d\n", disc.Profile, disc.Sectors)
	for _, t := range disc.Tracks {
		fmt.Fprintf(h, "track=%d audio=%v start=%d len=%d\n", t.Number, t.Audio, t.Start, t.Sectors)
	}
	label = disc.ProfileName
	if fsys, err := d.filesystem(); err == nil {
		v := fsys.Volume()
		fmt.Fprintf(h, "volume=%s created=%s\n", v.VolumeID, v.Created)
		if v.VolumeID != "" {
			label = v.VolumeID
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], label
}

func (s *server) scanDisc(ctx context.Context, rec *jobRecord, d *drive, disc *mmc.Disc, speedKB int) (*ScanResult, error) {
	dev, err := d.open()
	if err != nil {
		return nil, err
	}
	if speedKB > 0 {
		if err := dev.SetSpeed(speedKB); err != nil {
			rec.say("the drive would not accept a speed limit: %v", err)
		}
	}

	res := &ScanResult{Sectors: disc.Sectors, Map: make([]int, mapBuckets)}

	// C2 is a CD thing, and only if the drive will actually produce it for
	// this disc. Everything else is scanned for what can still be measured.
	sectorType := mmc.SectorAny
	if disc.AudioTracks > 0 && disc.DataTracks == 0 {
		sectorType = mmc.SectorCDDA
	}
	if disc.Profile.IsCD() && disc.RawReadable {
		res.C2Supported = dev.ProbeC2(startSector(disc), sectorType)
	}
	switch {
	case !disc.Profile.IsCD():
		res.Notes = append(res.Notes,
			"C2 error pointers exist only on CDs, so this scan reports unreadable sectors and read speed rather than an error rate")
	case !res.C2Supported:
		res.Notes = append(res.Notes,
			"this drive would not report C2 error pointers for this disc, so the scan reports unreadable sectors and read speed rather than an error rate")
	}

	rec.setPhase("reading the surface")
	res.Running = true
	rec.setScan(res)
	if res.C2Supported {
		err = s.scanWithC2(ctx, rec, dev, disc, sectorType, res)
	} else {
		err = s.scanPlain(ctx, rec, dev, disc, res)
	}
	res.Running = false
	if err != nil {
		return nil, err
	}

	rec.flushBad()
	res.BadRanges = rec.snapshot().BadRanges
	if res.NoC2 > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"%s read back but would not give their error flags - drives refuse that within a few sectors of the lead-out. Those sectors are fine; they are simply not measured.",
			plural(res.NoC2, "sector", "sectors")))
	}
	gradeDisc(res)
	return res, nil
}

func startSector(disc *mmc.Disc) int64 {
	if len(disc.Tracks) > 0 {
		return disc.Tracks[0].Start
	}
	return 0
}

// bucket maps a sector to its column in the strip.
func bucket(lba, sectors int64) int {
	if sectors <= 0 {
		return 0
	}
	b := int(lba * mapBuckets / sectors)
	if b >= mapBuckets {
		b = mapBuckets - 1
	}
	if b < 0 {
		b = 0
	}
	return b
}

// Severities the map uses where a byte count will not do.
const (
	sevUnreadable = -1 // the drive gave up on it
	sevStruggled  = -2 // the drive got it, slowly, by retrying
)

// worseThan orders the severities, so a bucket keeps the worst thing that
// happened in it: gone beats fought-for, and fought-for beats any number of
// bytes the correction quietly repaired.
func worseThan(a, b int) bool {
	rank := func(v int) int {
		switch v {
		case sevUnreadable:
			return 3
		case sevStruggled:
			return 2
		case 0:
			return 0
		}
		return 1
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra > rb
	}
	return a > b // two C2 counts: the bigger one
}

// note records a block's severity in the strip, keeping the worst.
func (r *ScanResult) note(lba int64, severity int) {
	b := bucket(lba, r.Sectors)
	if worseThan(severity, r.Map[b]) {
		r.Map[b] = severity
	}
}

// rates accumulates the per-block read rates so the average and the slowest
// can be reported. They are kept rather than averaged on the fly because
// the median is what "slow" has to be measured against, and a disc that is
// bad from its first sector would otherwise drag the average down to meet
// itself.
//
// Only whole blocks of the same size are sampled. A single-sector read - as
// the narrowing does - is dominated by the cost of the command rather than
// by the disc, and mixing those in makes every scan look as though the
// drive slowed down at the end, which is neither true nor useful.
type rateTracker struct {
	rates []float64
	// at is where each sample was read from, so a stretch the drive
	// struggled over can be put on the map rather than only counted. On a
	// DVD that is the only mark the map ever gets.
	at []int64
	// began is when the first read was offered. Until the drive is up to
	// speed its timings measure the motor rather than the disc.
	began time.Time
}

func (t *rateTracker) add(lba int64, bytes int64, took time.Duration) {
	if took <= 0 {
		return
	}
	now := time.Now()
	if t.began.IsZero() {
		t.began = now
	}
	// The reads at the start of a scan are not measurements of the disc:
	// the drive was stopped, and these wait for the motor. They come back
	// at a fraction of even 1x and differ from run to run - two scans of one
	// DVD reported 1334 and 203 kB/s for the same block, which is how this
	// was found. A scratch does not move, and it does not change depth
	// between Tuesday and Wednesday.
	//
	// The sectors are still read and still checked; it is only their timing
	// that is thrown away.
	if now.Sub(t.began) < spinUp {
		return
	}
	t.rates = append(t.rates, float64(bytes)/1024/took.Seconds())
	t.at = append(t.at, lba)
}

func (t *rateTracker) apply(res *ScanResult) {
	if len(t.rates) == 0 {
		return
	}
	sorted := append([]float64(nil), t.rates...)
	sort.Float64s(sorted)
	res.MinKBps = sorted[0]
	var sum float64
	for _, r := range t.rates {
		sum += r
	}
	res.AvgKBps = sum / float64(len(t.rates))
	prev := -2
	for _, i := range slowBlocks(t.rates) {
		res.SlowBlocks++
		if i != prev+1 {
			res.SlowStretches++
		}
		prev = i
		if i < len(t.at) {
			res.note(t.at[i], sevStruggled)
		}
	}
}

// slowBlocks finds the blocks that read far slower than the blocks around
// them. The rates are in the order they were read, which is the order they
// sit on the disc, so a window either side is a window of neighbouring
// track - the same radius, the same expected speed.
//
// A disc too short to have neighbours is not measured: with a handful of
// blocks there is nothing to be slower than.
func slowBlocks(rates []float64) []int {
	if len(rates) < 2*slowWindow+1 {
		return nil
	}
	window := make([]float64, 0, 2*slowWindow)
	var slow []int
	// A block within a window of either end has neighbours on one side
	// only, and against a disc that speeds up from the middle outwards that
	// is not enough to tell a dip from the ordinary ramp. Those blocks are
	// left unjudged rather than guessed at.
	for i := slowWindow; i < len(rates)-slowWindow; i++ {
		r := rates[i]
		lo, hi := i-slowWindow, i+slowWindow+1
		window = window[:0]
		for j := lo; j < hi; j++ {
			if j != i {
				window = append(window, rates[j])
			}
		}
		sort.Float64s(window)
		if local := window[len(window)/2]; r*slowFactor < local {
			slow = append(slow, i)
		}
	}
	return runsOf(slow, slowRun)
}

// runsOf keeps only the indices that belong to a run of at least n
// consecutive ones, and drops the isolated blocks that are noise.
func runsOf(idx []int, n int) []int {
	var out []int
	for i := 0; i < len(idx); {
		j := i + 1
		for j < len(idx) && idx[j] == idx[j-1]+1 {
			j++
		}
		if j-i >= n {
			out = append(out, idx[i:j]...)
		}
		i = j
	}
	return out
}

func (s *server) scanWithC2(ctx context.Context, rec *jobRecord, dev *mmc.Drive, disc *mmc.Disc, t mmc.SectorType, res *ScanResult) error {
	buf := make([]byte, mmc.ChunkC2*mmc.SectorRawC2)
	var rates rateTracker
	start := time.Now()

	for lba := int64(0); lba < disc.Sectors; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := mmc.ChunkC2
		if rest := disc.Sectors - lba; rest < int64(count) {
			count = int(rest)
		}
		began := time.Now()
		err := dev.ReadRawC2(lba, count, t, buf)
		took := time.Since(began)

		if err != nil {
			if !mmc.IsReadError(err) {
				return fmt.Errorf("checking sector %d: %w", lba, err)
			}
			// The block failed as a block; find out which sectors in it are
			// actually unreadable rather than writing the lot off.
			bad := s.narrowC2(ctx, dev, lba, count, t, res)
			rec.markBad(bad)
			res.Unreadable += int64(len(bad))
			lba += int64(count)
			res.blockDone(rec, lba)
			continue
		}
		if count == mmc.ChunkC2 {
			rates.add(lba, int64(count)*mmc.SectorRaw, took)
		}
		for i := range count {
			_, flags := mmc.SplitC2(buf, i)
			if n := mmc.CountC2(flags); n > 0 {
				res.C2Total += int64(n)
				res.C2Sectors++
				if n > res.C2Max {
					res.C2Max = n
				}
				res.note(lba+int64(i), n)
			}
		}
		lba += int64(count)
		res.blockDone(rec, lba)
	}
	res.ReadSeconds = time.Since(start).Seconds()
	res.Buckets = mapBuckets
	rates.apply(res)
	return nil
}

// blockDone is the bookkeeping both scan loops do after every block: how
// far the job has got, how much of the map has been filled, and publishing
// the two so a watching page can draw them.
//
// It is one function rather than five lines repeated twice because the two
// loops drifted the first time they were written - the DVD path was left
// publishing an empty map while it read, so a scan of a DVD showed a strip
// that never filled in.
func (r *ScanResult) blockDone(rec *jobRecord, lba int64) {
	rec.progress(lba * mmc.SectorData)
	r.advance(lba)
	rec.setScan(r)
}

// advance records how much of the disc the scan has covered, so a page
// drawing the strip can tell a clean bucket from one nobody has looked at
// yet.
func (r *ScanResult) advance(lba int64) {
	if r.Sectors <= 0 {
		r.Buckets = mapBuckets
		return
	}
	n := int(lba * mapBuckets / r.Sectors)
	if n > mapBuckets {
		n = mapBuckets
	}
	if n > r.Buckets {
		r.Buckets = n
	}
}

// narrowC2 reads a failed block one sector at a time, so a single bad
// sector costs one bad sector rather than the whole block.
//
// A sector that will not come back with its C2 flags is then asked for
// without them, and then as plain data. That distinction matters: a drive
// declines a C2 read near the lead-out, and on a disc that is otherwise
// perfect those last sectors would otherwise be reported as unreadable and
// the disc graded as failing. Only a sector that no read will produce is
// actually gone.
func (s *server) narrowC2(ctx context.Context, dev *mmc.Drive, lba int64, count int, t mmc.SectorType, res *ScanResult) []int64 {
	withC2 := make([]byte, mmc.SectorRawC2)
	raw := make([]byte, mmc.SectorRaw)
	data := make([]byte, mmc.SectorData)
	var bad []int64

	for i := range count {
		if ctx.Err() != nil {
			return bad
		}
		at := lba + int64(i)
		if err := dev.ReadRawC2(at, 1, t, withC2); err == nil {
			_, flags := mmc.SplitC2(withC2, 0)
			if n := mmc.CountC2(flags); n > 0 {
				res.C2Total += int64(n)
				res.C2Sectors++
				if n > res.C2Max {
					res.C2Max = n
				}
				res.note(at, n)
			}
			continue
		}
		// The flags were refused. The sector itself may be perfectly good.
		if err := dev.ReadRaw(at, 1, t, raw); err == nil {
			res.NoC2++
			continue
		}
		if err := dev.ReadData(at, 1, data); err == nil {
			res.NoC2++
			continue
		}
		bad = append(bad, at)
		res.note(at, sevUnreadable)
	}
	return bad
}

// scanPlain is the scan for a disc with no C2 to report: a DVD, or a drive
// that will not give them. It can still say which sectors are unreadable
// and where the drive slowed down, and a drive slowing down is the drive
// retrying, which is damage seen from the outside.
func (s *server) scanPlain(ctx context.Context, rec *jobRecord, dev *mmc.Drive, disc *mmc.Disc, res *ScanResult) error {
	buf := make([]byte, mmc.ChunkData*mmc.SectorData)
	var rates rateTracker
	start := time.Now()

	for lba := int64(0); lba < disc.Sectors; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := mmc.ChunkData
		if rest := disc.Sectors - lba; rest < int64(count) {
			count = int(rest)
		}
		began := time.Now()
		bad, err := dev.ReadDataRetry(lba, count, buf)
		took := time.Since(began)
		if err != nil {
			return fmt.Errorf("checking sector %d: %w", lba, err)
		}
		if len(bad) == 0 && count == mmc.ChunkData {
			rates.add(lba, int64(count)*mmc.SectorData, took)
		}
		for _, b := range bad {
			res.note(b, sevUnreadable)
		}
		rec.markBad(bad)
		res.Unreadable += int64(len(bad))
		lba += int64(count)
		res.blockDone(rec, lba)
	}
	res.ReadSeconds = time.Since(start).Seconds()
	res.Buckets = mapBuckets
	rates.apply(res)
	return nil
}

// Each grade owns a band of the score, so the word and the number can never
// disagree - a disc called failing must not score above one called
// degraded, however few of its sectors are actually gone.
const (
	scoreFailingTop  = 25.0
	scoreDegradedTop = 60.0
	scoreWornTop     = 90.0
)

// Where the bands sit in terms of the error rate: the fraction of sectors
// with at least one byte the correction could not fix.
const (
	rateGoodTop = 1e-4 // a handful on a full disc; normal for one that has been used
	rateWornTop = 1e-2 // one sector in a hundred; the disc is going
	rateFloor   = 0.2  // past here the score is at the bottom of its band
)

// gradeDisc turns the measurements into a word and a number.
//
// The thresholds are about the error rate rather than the raw count,
// because a 700 MB disc has three times the sectors of a 250 MB one and the
// same handful of flagged bytes means something different on each. They are
// deliberately cautious: "worn" is a disc to copy this month, not one that
// has failed. A disc with even one unreadable sector is failing, whatever
// the rest of it looks like - that sector is already gone.
func gradeDisc(res *ScanResult) {
	rate := 0.0
	if res.Sectors > 0 {
		rate = float64(res.C2Sectors) / float64(res.Sectors)
	}
	// band maps a position within a grade - 0 at its best, 1 at its worst -
	// onto that grade's score range.
	band := func(low, high, at float64) float64 {
		return high - (high-low)*math.Min(1, math.Max(0, at))
	}

	switch {
	case res.Unreadable > 0:
		lost := float64(res.Unreadable) / float64(max64(res.Sectors, 1))
		res.Grade = GradeFailing
		res.Score = band(0, scoreFailingTop, lost*100)
		res.Summary = fmt.Sprintf(
			"%s of this disc cannot be read any more. Copy what is left now: those sectors are gone and the rest is going the same way.",
			plural(res.Unreadable, "sector", "sectors"))

	case !res.C2Supported:
		res.Grade = GradeUnknown
		res.Score = 0
		res.Summary = "Every sector read back, and the drive never had to fight for one - but the error rate cannot be measured on this disc, so how much margin is left is unknown."
		if res.SlowStretches > 0 {
			res.Grade = GradeWorn
			res.Score = scoreWornTop
			res.Summary = fmt.Sprintf(
				"Every sector read back, but the drive slowed right down over %s - which is the drive retrying, and a sign of damage. Worth copying.",
				plural(int64(res.SlowStretches), "stretch", "stretches"))
		}

	case res.C2Sectors == 0:
		res.Grade = GradePristine
		res.Score = 100
		res.Summary = "Not one byte needed more than the ordinary error correction. This disc is as good as it was made."

	case rate < rateGoodTop:
		res.Grade = GradeGood
		res.Score = band(scoreWornTop, 100, rate/rateGoodTop)
		res.Summary = fmt.Sprintf(
			"%s of %s had a byte the error correction could not fix - a normal amount for a disc that has been used. Nothing to do.",
			plural(res.C2Sectors, "sector", "sectors"), plural(res.Sectors, "sector", "sectors"))

	case rate < rateWornTop:
		res.Grade = GradeWorn
		res.Score = band(scoreDegradedTop, scoreWornTop, (rate-rateGoodTop)/(rateWornTop-rateGoodTop))
		res.Summary = fmt.Sprintf(
			"%.2f%% of sectors have errors the correction had to repair. The disc still reads, but it is wearing out - copy it while it does.",
			rate*100)

	default:
		res.Grade = GradeDegraded
		res.Score = band(scoreFailingTop, scoreDegradedTop, (rate-rateWornTop)/(rateFloor-rateWornTop))
		res.Summary = fmt.Sprintf(
			"%.1f%% of sectors are damaged and being repaired on the fly. This disc is close to failing. Copy it today.",
			rate*100)
	}
	if res.SlowStretches > 0 && res.Grade == GradePristine {
		res.Grade = GradeGood
		res.Score = scoreWornTop
		res.Summary += " The drive did slow down in places, which usually means the disc is dirty rather than damaged - a wipe may be all it needs."
	}
	res.Score = math.Round(res.Score*10) / 10
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func plural(n int64, one, many string) string {
	word := many
	if n == 1 {
		word = one
	}
	return strconv.FormatInt(n, 10) + " " + word
}
