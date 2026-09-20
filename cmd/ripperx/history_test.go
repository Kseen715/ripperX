package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kseen715/ripperX/mmc"
)

func testHistory(t *testing.T) *history {
	t.Helper()
	h, err := openHistory(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.close() })
	return h
}

func saveTestScan(t *testing.T, h *history, id, disc string, at time.Time, c2, unreadable int64) {
	t.Helper()
	saveTestScanMeasured(t, h, id, disc, at, c2, unreadable, true)
}

func saveTestScanMeasured(t *testing.T, h *history, id, disc string, at time.Time, c2, unreadable int64, measured bool) {
	t.Helper()
	j := Job{ID: id, Kind: "scan", Drive: "sr0", Label: "check", State: jobDone, Started: at, Finished: at}
	res := &ScanResult{
		Sectors: 300000, C2Supported: measured, C2Sectors: c2, Unreadable: unreadable,
		Map: make([]int, mapBuckets),
	}
	gradeDisc(res)
	if err := h.saveScan(j, disc, "TEST DISC", &mmc.Disc{ProfileName: "CD-ROM"}, res); err != nil {
		t.Fatal(err)
	}
	// saveScan stamps scanned_at with the wall clock, so the ordering is
	// fixed up here to make a trend testable without waiting a year.
	if _, err := h.db.Exec("UPDATE scans SET scanned_at=? WHERE job_id=?", millis(at), id); err != nil {
		t.Fatal(err)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		found := false
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// The whole point of the database is that a restart does not lose what was
// learned, so the round trip is what has to be proved.
func TestJobsSurviveARoundTrip(t *testing.T) {
	h := testHistory(t)
	j := Job{
		ID: "abc123", Kind: "iso", Drive: "sr0", Label: "epson.iso from sr0",
		State: jobDone, Started: time.Now().Add(-time.Minute), Finished: time.Now(),
		Done: 1000, Total: 1000, BadSectors: 2, BadRanges: []string{"10-11"},
		SHA256: "deadbeef", Targets: []string{"epson.iso"}, Message: "done",
	}
	if err := h.saveJob(j); err != nil {
		t.Fatal(err)
	}
	// Saving the same job twice is what happens when a scan files itself and
	// then the job manager files it again; the second must not be a
	// duplicate row.
	if err := h.saveJob(j); err != nil {
		t.Fatal(err)
	}

	got, err := h.recentJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%d rows, want 1: saving twice created a duplicate", len(got))
	}
	if got[0].ID != j.ID || got[0].Kind != "iso" || got[0].SHA256 != "deadbeef" {
		t.Errorf("came back as %+v", got[0])
	}
	if len(got[0].Targets) != 1 || got[0].Targets[0] != "epson.iso" {
		t.Errorf("targets came back as %v", got[0].Targets)
	}
	if got[0].Started.IsZero() || got[0].Finished.IsZero() {
		t.Errorf("the times did not survive: %+v", got[0])
	}
}

// One disc scanned twice is the case the database exists for.
func TestDiscTrend(t *testing.T) {
	h := testHistory(t)
	year := 365 * 24 * time.Hour
	saveTestScan(t, h, "s1", "disc-a", time.Now().Add(-year), 10, 0)
	saveTestScan(t, h, "s2", "disc-a", time.Now(), 300, 0)

	discs, err := h.discs("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(discs) != 1 {
		t.Fatalf("%d discs, want 1: the two scans are of the same disc", len(discs))
	}
	d := discs[0]
	if len(d.Scans) != 2 {
		t.Fatalf("%d scans kept, want 2", len(d.Scans))
	}
	// Oldest first, so the trend reads left to right.
	if !d.Scans[0].ScannedAt.Before(d.Scans[1].ScannedAt) {
		t.Error("the scans are not in order")
	}
	if d.Trend != "worse" {
		t.Errorf("trend is %q, want worse: the error rate went up 300-fold", d.Trend)
	}
	if d.LatestGrade != GradeWorn {
		t.Errorf("latest grade is %q", d.LatestGrade)
	}
}

func TestDiscTrendFirstAndSteady(t *testing.T) {
	h := testHistory(t)
	saveTestScan(t, h, "s1", "disc-a", time.Now().Add(-48*time.Hour), 0, 0)
	discs, _ := h.discs("", false)
	if discs[0].Trend != "first" {
		t.Errorf("one scan should read as %q, got %q", "first", discs[0].Trend)
	}

	saveTestScan(t, h, "s2", "disc-a", time.Now(), 0, 0)
	discs, _ = h.discs("", false)
	if discs[0].Trend != "steady" {
		t.Errorf("two clean scans should read as steady, got %q", discs[0].Trend)
	}
}

// A sector that has become unreadable since the last check is the most
// urgent thing the history can say, and outranks the error rate.
func TestNewlyLostSectorsDominateTheTrend(t *testing.T) {
	h := testHistory(t)
	saveTestScan(t, h, "s1", "disc-a", time.Now().Add(-90*24*time.Hour), 500, 0)
	saveTestScan(t, h, "s2", "disc-a", time.Now(), 10, 4)
	discs, _ := h.discs("", false)
	if discs[0].Trend != "worse" {
		t.Errorf("trend is %q, want worse", discs[0].Trend)
	}
	if !containsAll(discs[0].TrendNote, "4 sectors", "unreadable") {
		t.Errorf("the note does not say what was lost: %q", discs[0].TrendNote)
	}
}

// Two discs are two discs, however similar their scans.
func TestDiscsAreKeptApart(t *testing.T) {
	h := testHistory(t)
	saveTestScan(t, h, "s1", "disc-a", time.Now().Add(-time.Hour), 0, 0)
	saveTestScan(t, h, "s2", "disc-b", time.Now(), 0, 0)
	discs, _ := h.discs("", false)
	if len(discs) != 2 {
		t.Fatalf("%d discs, want 2", len(discs))
	}
	// Most recently checked first.
	if discs[0].Disc != "disc-b" {
		t.Errorf("first disc is %q, want the one checked most recently", discs[0].Disc)
	}
	one, err := h.discs("disc-a", true)
	if err != nil || len(one) != 1 || one[0].Disc != "disc-a" {
		t.Fatalf("asking for one disc gave %v, %v", one, err)
	}
	if len(one[0].Scans[0].Map) != mapBuckets {
		t.Errorf("the damage map was not returned for a single disc: %d buckets",
			len(one[0].Scans[0].Map))
	}
}

// Every method has to be safe on a server with no database, because that is
// what a read-only filesystem produces and it must not stop a rip.
func TestNilHistoryIsHarmless(t *testing.T) {
	var h *history
	if err := h.saveJob(Job{ID: "x"}); err != nil {
		t.Errorf("saveJob on no database: %v", err)
	}
	if err := h.saveScan(Job{ID: "x"}, "d", "l", &mmc.Disc{}, &ScanResult{}); err != nil {
		t.Errorf("saveScan on no database: %v", err)
	}
	if jobs, err := h.recentJobs(10); err != nil || jobs != nil {
		t.Errorf("recentJobs on no database: %v, %v", jobs, err)
	}
	if discs, err := h.discs("", false); err != nil || discs != nil {
		t.Errorf("discs on no database: %v, %v", discs, err)
	}
	if err := h.close(); err != nil {
		t.Errorf("close on no database: %v", err)
	}
}

// A DVD's error rate cannot be measured at all, so two clean scans of one
// say nothing about whether it is decaying. Reporting "still not one
// uncorrected byte" would be claiming a measurement that was never made -
// which is exactly what it used to say.
func TestUnmeasurableDiscsDoNotClaimACleanBillOfHealth(t *testing.T) {
	h := testHistory(t)
	saveTestScanMeasured(t, h, "s1", "dvd", time.Now().Add(-90*24*time.Hour), 0, 0, false)
	saveTestScanMeasured(t, h, "s2", "dvd", time.Now(), 0, 0, false)

	discs, err := h.discs("", false)
	if err != nil {
		t.Fatal(err)
	}
	note := discs[0].TrendNote
	if strings.Contains(note, "uncorrected byte") {
		t.Errorf("the note claims an error count that was never taken: %q", note)
	}
	if !strings.Contains(note, "cannot be measured") {
		t.Errorf("the note does not say the measurement is unavailable: %q", note)
	}

	// A disc whose rate *is* measurable still gets the plain answer.
	h2 := testHistory(t)
	saveTestScan(t, h2, "s1", "cd", time.Now().Add(-90*24*time.Hour), 0, 0)
	saveTestScan(t, h2, "s2", "cd", time.Now(), 0, 0)
	discs, _ = h2.discs("", false)
	if !strings.Contains(discs[0].TrendNote, "uncorrected byte") {
		t.Errorf("a measured disc lost its plain answer: %q", discs[0].TrendNote)
	}
}

func TestRoughly(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30 minutes"},
		{5 * time.Hour, "5 hours"},
		{10 * 24 * time.Hour, "10 days"},
		{100 * 24 * time.Hour, "3 months"},
		{800 * 24 * time.Hour, "2 years"},
	}
	for _, tc := range cases {
		if got := roughly(tc.d); got != tc.want {
			t.Errorf("roughly(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
