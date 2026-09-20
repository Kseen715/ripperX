package main

import (
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
