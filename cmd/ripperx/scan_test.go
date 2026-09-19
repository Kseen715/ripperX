package main

import (
	"strings"
	"testing"
	"time"
)

// The grade is the one word a person reads, so each threshold is pinned
// here. They are deliberately cautious: "worn" has to mean a disc to copy
// this month, not one that has already failed.
func TestGradeDisc(t *testing.T) {
	cases := []struct {
		name string
		in   ScanResult
		want Grade
	}{
		{"clean disc", ScanResult{Sectors: 300000, C2Supported: true}, GradePristine},
		{"a handful of flagged sectors", ScanResult{
			Sectors: 300000, C2Supported: true, C2Sectors: 10, C2Total: 40}, GradeGood},
		{"a real error rate", ScanResult{
			Sectors: 300000, C2Supported: true, C2Sectors: 300}, GradeWorn},
		{"exactly at the worn/degraded boundary", ScanResult{
			Sectors: 300000, C2Supported: true, C2Sectors: 3000}, GradeDegraded},
		{"heavy damage", ScanResult{
			Sectors: 300000, C2Supported: true, C2Sectors: 90000}, GradeDegraded},
		{"one sector already gone", ScanResult{
			Sectors: 300000, C2Supported: true, Unreadable: 1}, GradeFailing},
		{"nothing measurable", ScanResult{Sectors: 300000}, GradeUnknown},
		{"nothing measurable but the drive struggled", ScanResult{
			Sectors: 300000, SlowBlocks: 4, SlowStretches: 1}, GradeWorn},
	}
	for _, tc := range cases {
		got := tc.in
		got.Map = make([]int, mapBuckets)
		gradeDisc(&got)
		if got.Grade != tc.want {
			t.Errorf("%s: graded %q, want %q", tc.name, got.Grade, tc.want)
		}
		if got.Summary == "" {
			t.Errorf("%s: no summary, which is the part a person reads", tc.name)
		}
		if got.Score < 0 || got.Score > 100 {
			t.Errorf("%s: score %v is outside 0..100", tc.name, got.Score)
		}
	}
}

// An unreadable sector outranks everything: the data there is already gone,
// however clean the rest of the disc looks.
func TestUnreadableOutranksAGoodErrorRate(t *testing.T) {
	r := ScanResult{Sectors: 300000, C2Supported: true, Unreadable: 3, Map: make([]int, mapBuckets)}
	gradeDisc(&r)
	if r.Grade != GradeFailing {
		t.Errorf("graded %q, want failing", r.Grade)
	}
	if !strings.Contains(r.Summary, "Copy") {
		t.Errorf("the summary does not tell the user to act: %q", r.Summary)
	}
}

// A worse disc must never score higher than a better one, whatever the
// branch each lands in.
func TestScoreFallsAsTheDiscGetsWorse(t *testing.T) {
	score := func(c2 int64, unreadable int64) float64 {
		r := ScanResult{Sectors: 300000, C2Supported: true, C2Sectors: c2,
			Unreadable: unreadable, Map: make([]int, mapBuckets)}
		gradeDisc(&r)
		return r.Score
	}
	steps := []float64{score(0, 0), score(10, 0), score(300, 0), score(3000, 0),
		score(90000, 0), score(0, 5), score(0, 5000)}
	for i := 1; i < len(steps); i++ {
		if steps[i] > steps[i-1] {
			t.Errorf("step %d scored %v, higher than the better disc before it at %v",
				i, steps[i], steps[i-1])
		}
	}
}

func TestBucketing(t *testing.T) {
	if got := bucket(0, 1000); got != 0 {
		t.Errorf("the first sector is in bucket %d", got)
	}
	if got := bucket(999, 1000); got != mapBuckets-1 {
		t.Errorf("the last sector is in bucket %d, want %d", got, mapBuckets-1)
	}
	// A sector past the end, or a disc of no length, must not index out of
	// the slice.
	if got := bucket(5000, 1000); got != mapBuckets-1 {
		t.Errorf("a sector past the end landed in bucket %d", got)
	}
	if got := bucket(5, 0); got != 0 {
		t.Errorf("a disc of no length gave bucket %d", got)
	}
}

// The map keeps the worst thing seen in each bucket, and unreadable is the
// worst thing there is.
func TestMapKeepsTheWorst(t *testing.T) {
	r := ScanResult{Sectors: mapBuckets, Map: make([]int, mapBuckets)}
	r.note(0, 4)
	r.note(0, 40)
	r.note(0, 2)
	if r.Map[0] != 40 {
		t.Errorf("bucket 0 is %d, want the worst of the three", r.Map[0])
	}
	r.note(0, sevStruggled)
	if r.Map[0] != sevStruggled {
		t.Errorf("bucket 0 is %d, want a stretch the drive fought over to outrank repaired bytes", r.Map[0])
	}
	r.note(0, sevUnreadable)
	if r.Map[0] != sevUnreadable {
		t.Errorf("bucket 0 is %d, want unreadable to win", r.Map[0])
	}
	r.note(0, 900)
	if r.Map[0] != sevUnreadable {
		t.Errorf("bucket 0 is %d; nothing outranks unreadable", r.Map[0])
	}
	r.note(0, sevStruggled)
	if r.Map[0] != sevUnreadable {
		t.Errorf("bucket 0 is %d; a slow read does not undo a lost sector", r.Map[0])
	}
}

// The rates themselves: what was recorded, and the average and minimum
// taken from it. Whether a block counts as slow is a separate question with
// its own tests, because it depends on the blocks around it rather than on
// this arithmetic.
func TestRateTracker(t *testing.T) {
	var rt rateTracker
	// Past the spin-up, so the samples are of the disc rather than of the
	// motor; that rule has its own test.
	rt.began = time.Now().Add(-spinUp - time.Second)

	// Four blocks of a megabyte in 100 ms, and one that took ten times as
	// long.
	for range 4 {
		rt.add(int64(0), 1<<20, 100*time.Millisecond)
	}
	rt.add(int64(0), 1<<20, time.Second)

	res := &ScanResult{Sectors: 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.MinKBps >= res.AvgKBps {
		t.Errorf("the slowest block (%v) is not below the average (%v)", res.MinKBps, res.AvgKBps)
	}
	if got := res.MinKBps; got < 1000 || got > 1060 {
		t.Errorf("the slowest block reads %v kB/s, want about 1024", got)
	}
	// A read that took no measurable time is not a rate; it is a clock too
	// coarse to see it, and averaging infinity in would ruin the figure.
	before := len(rt.rates)
	rt.add(int64(0), 1<<20, 0)
	if len(rt.rates) != before {
		t.Error("a zero-length read was recorded as a rate")
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "sector", "sectors"); got != "1 sector" {
		t.Errorf("got %q", got)
	}
	if got := plural(0, "sector", "sectors"); got != "0 sectors" {
		t.Errorf("got %q", got)
	}
}

// While a scan is running the buckets it has not reached are not clean,
// they are unread - and the page has to be able to tell them apart or it
// shows a disc a clean bill of health it has not earned.
func TestScanAdvanceTracksHowFarItHasGot(t *testing.T) {
	r := &ScanResult{Sectors: 1000, Map: make([]int, mapBuckets)}
	if r.Buckets != 0 {
		t.Errorf("a scan starts having read %d buckets", r.Buckets)
	}
	r.advance(500)
	if r.Buckets != mapBuckets/2 {
		t.Errorf("halfway is %d buckets, want %d", r.Buckets, mapBuckets/2)
	}
	// It only ever goes forwards, whatever order the blocks finish in.
	r.advance(100)
	if r.Buckets != mapBuckets/2 {
		t.Errorf("the count went backwards to %d", r.Buckets)
	}
	r.advance(1000)
	if r.Buckets != mapBuckets {
		t.Errorf("the end is %d buckets, want %d", r.Buckets, mapBuckets)
	}
	// A disc of no length must not divide by it.
	empty := &ScanResult{Map: make([]int, mapBuckets)}
	empty.advance(0)
	if empty.Buckets != mapBuckets {
		t.Errorf("an empty disc reported %d buckets", empty.Buckets)
	}
}

// Both scan loops report through blockDone, so one test covers both. It
// exists because they did not always: the DVD path was left publishing an
// empty map while it read, and a strip that never fills in looks exactly
// like a scan that is not working.
func TestBlockDoneReportsEverything(t *testing.T) {
	rec := &jobRecord{}
	res := &ScanResult{Sectors: 1000, Map: make([]int, mapBuckets)}

	res.blockDone(rec, 500)

	j := rec.snapshot()
	if j.Done != 500*2048 {
		t.Errorf("progress is %d bytes, want %d", j.Done, 500*2048)
	}
	if res.Buckets != mapBuckets/2 {
		t.Errorf("the map has %d buckets read, want %d", res.Buckets, mapBuckets/2)
	}
	if j.Scan == nil {
		t.Fatal("nothing was published for a watching page to draw")
	}
	if j.Scan.Buckets != mapBuckets/2 {
		t.Errorf("the published copy says %d buckets, want %d", j.Scan.Buckets, mapBuckets/2)
	}
	if len(j.Scan.Map) != mapBuckets {
		t.Errorf("the published map is %d buckets long", len(j.Scan.Map))
	}
}

// An optical drive spins at constant angular velocity, so a read from the
// middle of a disc outwards speeds up by a factor of two or three with
// nothing wrong at all. Measured against one figure for the whole disc that
// ramp looks exactly like damage, and a perfectly good DVD gets called worn.
func TestSpeedRampIsNotDamage(t *testing.T) {
	var rt rateTracker
	// A clean DVD: 2000 kB/s at the centre rising to 8000 at the rim.
	for i := range 400 {
		rt.rates = append(rt.rates, 2000+float64(i)*15)
		rt.at = append(rt.at, int64(i)*1000)
	}
	res := &ScanResult{}
	rt.apply(res)
	if res.SlowBlocks != 0 {
		t.Errorf("%d blocks called slow on a disc that only got faster", res.SlowBlocks)
	}
	if res.MinKBps != 2000 {
		t.Errorf("slowest block reported as %v, want the innermost at 2000", res.MinKBps)
	}
}

// A stretch that is slow compared with the track either side of it is the
// real signal, and it has to survive the ramp.
func TestALocalDipIsFound(t *testing.T) {
	var rt rateTracker
	for i := range 400 {
		rt.rates = append(rt.rates, 2000+float64(i)*15)
		rt.at = append(rt.at, int64(i)*1000)
	}
	// A scratch: a run of blocks at a twentieth of what their neighbours
	// manage. One block on its own is noise and is ignored on purpose.
	for i := 200; i < 200+slowRun; i++ {
		rt.rates[i] /= 20
	}
	res := &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != slowRun {
		t.Errorf("%d slow blocks, want the %d of the dip", res.SlowBlocks, slowRun)
	}
	// And it has to show on the map: on a DVD there are no C2 pointers, so a
	// stretch the drive fought over is the only mark the map ever gets, and
	// a count with no position is no use for finding the scratch.
	if res.Map[bucket(200*1000, res.Sectors)] != sevStruggled {
		t.Error("the mark is not where the slow stretch was")
	}
}

// One slow block on its own is the host being busy, not a scratch. It
// produced three false alarms before this rule went in and never once
// matched a real defect.
func TestALoneSlowBlockIsIgnored(t *testing.T) {
	var rt rateTracker
	for i := range 400 {
		rt.rates = append(rt.rates, 5000)
		rt.at = append(rt.at, int64(i)*1000)
	}
	rt.rates[200] = 50

	res := &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != 0 {
		t.Errorf("%d slow blocks, want none: one block is not a scratch", res.SlowBlocks)
	}
}

func TestRunsOf(t *testing.T) {
	cases := []struct {
		in   []int
		want []int
	}{
		{nil, nil},
		{[]int{5}, nil},
		{[]int{5, 6}, nil},
		{[]int{5, 6, 7}, []int{5, 6, 7}},
		{[]int{1, 5, 6, 7, 20}, []int{5, 6, 7}},
		{[]int{1, 2, 3, 9, 10, 11, 12}, []int{1, 2, 3, 9, 10, 11, 12}},
	}
	for _, tc := range cases {
		got := runsOf(tc.in, 3)
		if len(got) != len(tc.want) {
			t.Errorf("runsOf(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("runsOf(%v) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// A run of bad blocks - a scratch - is several, not one.
func TestAScratchIsSeveralSlowBlocks(t *testing.T) {
	var rt rateTracker
	for i := range 400 {
		rt.rates = append(rt.rates, 5000)
		rt.at = append(rt.at, int64(i)*1000)
	}
	for i := 200; i < 205; i++ {
		rt.rates[i] = 100
	}
	res := &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != 5 {
		t.Errorf("%d slow blocks, want 5", res.SlowBlocks)
	}
}

// Too few blocks to have neighbours means nothing can be said, and saying
// nothing is better than calling a short disc damaged.
func TestATinyDiscIsNotJudgedOnSpeed(t *testing.T) {
	var rt rateTracker
	rt.rates = []float64{5000, 100, 5000}
	rt.at = []int64{0, 1000, 2000}
	res := &ScanResult{Sectors: 3000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != 0 {
		t.Errorf("%d slow blocks on a disc with no neighbours to compare against", res.SlowBlocks)
	}
}

// The first read of a scan waits for a stopped drive to spin up, so it
// comes back at a fraction of even 1x and varies from run to run. Counting
// it condemned a disc for having a motor: two scans of one DVD reported
// 1334 and 203 kB/s for the same block, and graded it worn both times.
func TestSpinUpIsNotMeasured(t *testing.T) {
	var rt rateTracker

	// While the drive is coming up to speed, nothing is recorded.
	rt.add(0, 1<<20, time.Second)
	rt.add(64, 1<<20, 900*time.Millisecond)
	if len(rt.rates) != 0 {
		t.Fatalf("%d samples kept during spin-up, want none", len(rt.rates))
	}

	// Once it is up to speed - which the test reaches by moving the start
	// back rather than by waiting three seconds - the disc is measured.
	rt.began = time.Now().Add(-spinUp - time.Second)
	rt.add(128, 1<<20, 100*time.Millisecond)
	rt.add(192, 1<<20, 100*time.Millisecond)
	if len(rt.rates) != 2 {
		t.Fatalf("%d samples kept after spin-up, want 2", len(rt.rates))
	}

	res := &ScanResult{Sectors: 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.MinKBps < 10000 {
		t.Errorf("the slowest block is %v kB/s: a spin-up read is still in the figures", res.MinKBps)
	}
}

// A block at either end of the disc has neighbours on one side only. On a
// disc that legitimately speeds up from the middle outwards that is not
// enough to tell a dip from the ramp, so those are left unjudged rather
// than guessed at.
func TestEdgeBlocksAreNotJudged(t *testing.T) {
	var rt rateTracker
	for i := range 400 {
		rt.rates = append(rt.rates, 5000)
		rt.at = append(rt.at, int64(i)*1000)
	}
	// A dip at the very start and one at the very end, each long enough to
	// be reported were it anywhere else.
	for i := range slowRun {
		rt.rates[i] = 50
		rt.rates[len(rt.rates)-1-i] = 50
	}

	res := &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != 0 {
		t.Errorf("%d slow blocks, want none: both dips are at an edge", res.SlowBlocks)
	}

	// The same dip in the middle of the disc is still found.
	for i := 200; i < 200+slowRun; i++ {
		rt.rates[i] = 50
	}
	res = &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)
	if res.SlowBlocks != slowRun {
		t.Errorf("%d slow blocks, want the %d in the middle", res.SlowBlocks, slowRun)
	}
}

// One scratch is several slow blocks but it is still one scratch, and that
// is the number to put in front of a person.
func TestSlowStretchesAreCountedSeparatelyFromBlocks(t *testing.T) {
	var rt rateTracker
	for i := range 400 {
		rt.rates = append(rt.rates, 5000)
		rt.at = append(rt.at, int64(i)*1000)
	}
	// Two scratches, four blocks each.
	for _, start := range []int{100, 300} {
		for i := start; i < start+4; i++ {
			rt.rates[i] = 50
		}
	}
	res := &ScanResult{Sectors: 400 * 1000, Map: make([]int, mapBuckets)}
	rt.apply(res)

	if res.SlowBlocks != 8 {
		t.Errorf("%d slow blocks, want 8", res.SlowBlocks)
	}
	if res.SlowStretches != 2 {
		t.Errorf("%d stretches, want 2", res.SlowStretches)
	}
	gradeDisc(res)
	if !strings.Contains(res.Summary, "2 stretches") {
		t.Errorf("the summary counts blocks rather than stretches: %q", res.Summary)
	}
}
