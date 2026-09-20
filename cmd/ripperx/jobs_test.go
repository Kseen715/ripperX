package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

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
