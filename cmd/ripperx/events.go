package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Every page watching this server sees the same thing: the same drives,
// the same discs, the same jobs at the same percentage. That is what makes
// a rip started on a laptop visible on a phone, and what makes two people
// unable to start the same rip twice without noticing.
//
// The mechanism is server-sent events carrying whole snapshots rather than
// deltas, so a browser that reconnects or missed a frame is correct again
// with the next message and needs no reconciliation logic at all.

const (
	// tick is how often the picture is rebuilt and, if it changed, sent.
	// Fast enough that a progress bar moves smoothly, slow enough that a
	// dozen open tabs cost nothing.
	tick = 500 * time.Millisecond
	// keepalive keeps a proxy from closing a stream that has had nothing
	// to say for a while.
	keepalive = 25 * time.Second
)

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}

	// build produces the current snapshot. It is set on the first publish
	// and is the same function thereafter; holding it here is what lets the
	// ticker rebuild without the job manager having to drive it.
	build func() any
	last  []byte

	started bool
}

func newHub() *hub {
	return &hub{subs: map[chan []byte]struct{}{}}
}

// publish notes how to build a snapshot and wakes the loop. Callers use it
// to say "something changed"; the loop decides when to actually send, so a
// rip reporting progress a hundred times a second costs a hundred cheap
// calls and two messages.
func (h *hub) publish(build func() any) {
	h.mu.Lock()
	h.build = build
	if !h.started {
		h.started = true
		go h.loop()
	}
	h.mu.Unlock()
}

func (h *hub) loop() {
	t := time.NewTicker(tick)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		build, n := h.build, len(h.subs)
		h.mu.Unlock()
		if build == nil || n == 0 {
			continue
		}
		body, err := json.Marshal(build())
		if err != nil {
			continue
		}
		h.mu.Lock()
		// Nothing changed since the last message: say nothing. An idle
		// server with ten pages open sends nothing at all.
		if bytes.Equal(body, h.last) {
			h.mu.Unlock()
			continue
		}
		h.last = body
		for ch := range h.subs {
			select {
			case ch <- body:
			default:
				// A page too slow to keep up is skipped rather than waited
				// for; the next snapshot is whole, so it loses nothing but
				// one frame.
			}
		}
		h.mu.Unlock()
	}
}

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	last := h.last
	h.mu.Unlock()
	if last != nil {
		ch <- last
	}
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError,
			fmt.Errorf("this connection cannot be streamed"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// A reverse proxy that buffers would hold every frame until the rip
	// finished, which is exactly the opposite of the point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The first frame goes out immediately, so a page that has just loaded
	// is not blank until something changes.
	h := s.jobs.hub
	h.publish(func() any { return s.snapshot() })
	ch := h.subscribe()
	defer h.unsubscribe(ch)

	if body, err := json.Marshal(s.snapshot()); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", body)
		flusher.Flush()
	}

	idle := time.NewTicker(keepalive)
	defer idle.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case body := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", body)
			flusher.Flush()
		case <-idle.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
