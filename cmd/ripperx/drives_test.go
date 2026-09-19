package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Kseen715/ripperX/mmc"
)

// The reservation exists so that a second rip is refused while it is still a
// request, rather than becoming a job that fails a moment later. That is a
// race by nature, so it is worth proving rather than reasoning about.
func TestOnlyOneJobCanHaveADrive(t *testing.T) {
	d := &drive{id: "sr0", path: "/dev/null"}
	if err := d.reserve(); err != nil {
		t.Fatalf("the first reservation must succeed: %v", err)
	}
	var busy *errDriveBusy
	if err := d.reserve(); !errors.As(err, &busy) {
		t.Fatalf("the second reservation gave %v, want a busy error", err)
	}
	if got := d.heldBy(); got != pendingJob {
		t.Errorf("heldBy = %q while reserved, want %q", got, pendingJob)
	}

	d.hold("job1")
	if got := d.heldBy(); got != "job1" {
		t.Errorf("heldBy = %q once held, want the job id", got)
	}
	d.release()
	if got := d.heldBy(); got != "" {
		t.Errorf("heldBy = %q after release, want empty", got)
	}
	if err := d.reserve(); err != nil {
		t.Errorf("the drive should be free again: %v", err)
	}
	d.unreserve()
}

func TestUnreserveOnlyClearsAReservation(t *testing.T) {
	d := &drive{id: "sr0"}
	if err := d.reserve(); err != nil {
		t.Fatal(err)
	}
	d.hold("job1")
	d.unreserve() // must not steal the drive from the job that now holds it
	if got := d.heldBy(); got != "job1" {
		t.Errorf("heldBy = %q, want the job still holding it", got)
	}
	d.release()
}

func TestReserveUnderContention(t *testing.T) {
	d := &drive{id: "sr0"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.reserve(); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d goroutines reserved the drive, want exactly 1", won)
	}
}

// A job that never starts must give the drive back, or the drive is stuck
// until the process restarts.
func TestStartOnDriveReleasesWhenTheDriveIsTaken(t *testing.T) {
	s := &server{drives: newDriveSet([]string{"/dev/sr0"})}
	s.jobs = newJobManager(s)
	d, err := s.drives.get("sr0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	if _, err := s.jobs.startOnDrive(d, "iso", "first", 0,
		func(ctx context.Context, rec *jobRecord) error {
			<-done
			return nil
		}); err != nil {
		t.Fatalf("the first job should have got the drive: %v", err)
	}
	// The job's goroutine has to reach hold() before the drive reads as busy.
	waitFor(t, func() bool { return d.heldBy() != "" })

	if _, err := s.jobs.startOnDrive(d, "iso", "second", 0,
		func(context.Context, *jobRecord) error { return nil }); err == nil {
		t.Error("the second job took a drive that was already held")
	}
	if n := len(s.jobs.list()); n != 1 {
		t.Errorf("%d jobs exist, want 1: a refused start must not leave a failed job behind", n)
	}

	close(done)
	waitFor(t, func() bool { return d.heldBy() == "" })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the drive to change hands")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDriveIDs(t *testing.T) {
	ds := newDriveSet([]string{"/dev/sr0", "/dev/sr1", "/dev/sr0"})
	if ds.count() != 2 {
		t.Errorf("the same node listed twice produced %d drives, want 2", ds.count())
	}
	if _, err := ds.get("sr1"); err != nil {
		t.Errorf("sr1 should be addressable: %v", err)
	}
	if _, err := ds.get("sr9"); !errors.Is(err, errNoSuchDrive) {
		t.Errorf("an unknown drive gave %v, want errNoSuchDrive", err)
	}
}

// Unreadable sectors are reported as ranges because a scratch produces a run
// of thousands and listing each one helps nobody.
func TestBadSectorsCoalesceIntoRanges(t *testing.T) {
	rec := &jobRecord{}
	rec.markBad([]int64{10, 11, 12})
	rec.markBad([]int64{13})
	rec.markBad([]int64{100})
	rec.finish(nil, false)

	j := rec.snapshot()
	if j.BadSectors != 5 {
		t.Errorf("counted %d bad sectors, want 5", j.BadSectors)
	}
	want := []string{"10-13", "100"}
	if len(j.BadRanges) != len(want) {
		t.Fatalf("ranges %v, want %v", j.BadRanges, want)
	}
	for i := range want {
		if j.BadRanges[i] != want[i] {
			t.Errorf("range %d is %q, want %q", i, j.BadRanges[i], want[i])
		}
	}
}

func TestJobStatesAreReported(t *testing.T) {
	cancelled := &jobRecord{}
	cancelled.finish(context.Canceled, true)
	if got := cancelled.snapshot().State; got != jobCancelled {
		t.Errorf("a cancelled job is %q, want %q", got, jobCancelled)
	}

	failed := &jobRecord{}
	failed.finish(errors.New("the drive fell over"), false)
	snap := failed.snapshot()
	if snap.State != jobFailed || snap.Error != "the drive fell over" {
		t.Errorf("a failed job is %q/%q", snap.State, snap.Error)
	}

	ok := &jobRecord{}
	ok.finish(nil, false)
	if got := ok.snapshot().State; got != jobDone {
		t.Errorf("a finished job is %q, want %q", got, jobDone)
	}
}

// A download holds the drive for reading. A rip asked for while one is
// running must be refused with a reason, not left to block: before this,
// the reservation succeeded and the job's goroutine then waited on the
// lock, so the page showed a rip "running" that was doing nothing at all.
func TestRipDuringADownloadIsRefusedNotStalled(t *testing.T) {
	d := &drive{id: "sr0", path: "/dev/null"}

	streaming := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.rw.RLock() // what borrow holds while a file is being sent
		close(streaming)
		<-done
		d.rw.RUnlock()
	}()
	<-streaming

	err := d.reserve()
	if err == nil {
		t.Fatal("the reservation succeeded while the drive was streaming")
	}
	if !errors.Is(err, errDriveStreaming) {
		t.Errorf("refused with %v, want the streaming error", err)
	}

	close(done)
	// Once the download is over the drive is free again.
	waitFor(t, func() bool { return d.reserve() == nil })
	d.unreserve()
	if err := d.reserve(); err != nil {
		t.Errorf("the drive did not come back: %v", err)
	}
	d.unreserve()
}

// And the other way round: a listing asked for while a job has the drive
// answers rather than waiting for the rip to end.
func TestBorrowDuringAJobAnswersAtOnce(t *testing.T) {
	d := &drive{id: "sr0", path: "/dev/null"}
	if err := d.reserve(); err != nil {
		t.Fatal(err)
	}
	d.hold("job1")
	defer d.release()

	err := d.borrow(func(*mmc.Drive) error { return nil })
	var busy *errDriveBusy
	if !errors.As(err, &busy) {
		t.Errorf("borrow gave %v, want a busy error naming the job", err)
	}
}

// A reservation that is abandoned has to give the device back, or the drive
// is locked until the process restarts.
func TestUnreserveReleasesTheDevice(t *testing.T) {
	d := &drive{id: "sr0", path: "/dev/null"}
	if err := d.reserve(); err != nil {
		t.Fatal(err)
	}
	d.unreserve()
	// If the write lock were still held this would not return.
	if err := d.borrow(func(*mmc.Drive) error { return nil }); err != nil {
		t.Errorf("the drive is still locked after an abandoned reservation: %v", err)
	}
}

// The estimate is what a person plans around, so it has to be absent rather
// than wrong when there is nothing to base it on.
func TestETA(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		job  Job
		want float64 // 0 means "there should be no estimate"
	}{
		{"not started", Job{State: jobRunning, Total: 100}, 0},
		{"no total known", Job{State: jobRunning, Done: 50}, 0},
		{"already finished", Job{State: jobDone, Done: 100, Total: 100}, 0},
		{"done equals total", Job{State: jobRunning, Done: 100, Total: 100}, 0},
		{"from the recent rate", Job{
			State: jobRunning, Done: 100, Total: 300, BytesPerSec: 50}, 4},
		{"from the average when there is no recent rate", Job{
			State: jobRunning, Done: 100, Total: 300, Started: now.Add(-10 * time.Second)}, 20},
	}
	for _, tc := range cases {
		got := tc.job.eta()
		if tc.want == 0 && got != 0 {
			t.Errorf("%s: estimated %v seconds, want no estimate", tc.name, got)
			continue
		}
		if tc.want != 0 && (got < tc.want*0.8 || got > tc.want*1.2) {
			t.Errorf("%s: estimated %v seconds, want about %v", tc.name, got, tc.want)
		}
	}
}

// A scan publishes its partial result on every block while the event hub
// marshals the last snapshot. Sharing the slice between the two is a data
// race; this fails under -race if setScan ever stops copying.
func TestScanResultIsCopiedNotShared(t *testing.T) {
	rec := &jobRecord{}
	res := &ScanResult{Sectors: 1000, Map: make([]int, mapBuckets)}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// What the hub does: take a snapshot and read everything in it.
			if s := rec.snapshot(); s.Scan != nil {
				total := 0
				for _, v := range s.Scan.Map {
					total += v
				}
				_ = total
			}
		}
	}()

	for i := range mapBuckets {
		res.Map[i] = i
		res.advance(int64(i) * 1000 / mapBuckets)
		rec.setScan(res)
	}
	close(stop)
	wg.Wait()

	if got := rec.snapshot().Scan; got == nil || len(got.Map) != mapBuckets {
		t.Fatalf("the scan did not survive: %+v", got)
	}
}

// The event stream takes a status read of every drive twice a second, so a
// reservation that gave up the instant it found the lock held would refuse
// rips at random. A short read has to be waited out; only a long one is a
// refusal.
func TestReserveWaitsOutAShortRead(t *testing.T) {
	d := &drive{id: "sr0", path: "/dev/null"}

	holding := make(chan struct{})
	go func() {
		d.rw.RLock()
		close(holding)
		time.Sleep(150 * time.Millisecond) // a status read, not a download
		d.rw.RUnlock()
	}()
	<-holding

	start := time.Now()
	if err := d.reserve(); err != nil {
		t.Fatalf("a rip was refused because a status read was in flight: %v", err)
	}
	if took := time.Since(start); took > reserveWait {
		t.Errorf("waited %v, which is longer than it should ever wait", took)
	}
	d.unreserve()
}
