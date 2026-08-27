package mirror

import (
	"sync"
	"time"
)

// timelineWindow and timelineMax bound the ring two ways. The window is what an
// operator is looking at; the count is the memory ceiling that holds whatever
// the traffic does inside it.
const (
	timelineWindow = 24 * time.Hour
	timelineMax    = 100000
)

// Frame is one timed event on the chart: what the mirror exchanged, when, and
// what it cost.
type Frame struct {
	Lane      Lane          `json:"lane"`
	Method    string        `json:"method"`
	Path      string        `json:"path"`
	Status    int           `json:"status"`
	Bytes     int           `json:"bytes"`
	At        time.Time     `json:"at"`
	Duration  time.Duration `json:"duration_ns"`
	Principal string        `json:"principal,omitempty"`
	Detail    string        `json:"detail,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// Timeline is a bounded, in-memory ring of everything the mirror exchanged.
//
// It is deliberately memory-only. This is a live view, not an audit log: a
// table would put sub-day-ephemeral data behind a schema whose change nukes the
// cache, and nothing here is worth that. It resets on restart, and says so.
type Timeline struct {
	mu      sync.Mutex
	frames  []Frame
	head    int
	size    int
	dropped int
	started time.Time
	now     func() time.Time
}

// NewTimeline returns an empty ring sized to the ceiling.
func NewTimeline() *Timeline {
	return &Timeline{
		frames:  make([]Frame, timelineMax),
		started: time.Now(),
		now:     time.Now,
	}
}

// Observe records one exchange. It never blocks on anything but its own lock,
// because the caller is on the path of the request being measured.
func (t *Timeline) Observe(e Exchange) {
	if t == nil {
		return
	}
	f := Frame{
		Lane:      e.Lane,
		Method:    e.Method,
		Path:      e.Path,
		Status:    e.Status,
		Bytes:     e.Bytes,
		At:        e.Started,
		Duration:  e.Duration,
		Principal: e.Principal,
		Detail:    e.Detail,
	}
	if f.Path == "" {
		f.Path = pathOf(e.URL)
	}
	if f.At.IsZero() {
		f.At = t.now()
	}
	if e.Err != nil {
		f.Error = e.Err.Error()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.size == len(t.frames) {
		t.dropped++
		t.frames[t.head] = f
		t.head = (t.head + 1) % len(t.frames)
		return
	}
	t.frames[(t.head+t.size)%len(t.frames)] = f
	t.size++
}

// Frames returns what is still inside the window, oldest first.
//
// Eviction is lazy, here, rather than on a timer: a background goroutine that
// only ever deletes is a moving part with nothing to gain from moving.
func (t *Timeline) Frames() []Frame {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-timelineWindow)
	out := make([]Frame, 0, t.size)
	live := 0
	for i := range t.size {
		f := t.frames[(t.head+i)%len(t.frames)]
		if f.At.Before(cutoff) {
			continue
		}
		if live == 0 {
			// Everything before this one is out of the window; move the head past
			// them so the next sweep does not walk them again.
			t.head = (t.head + i) % len(t.frames)
		}
		live++
		out = append(out, f)
	}
	t.size = live
	return out
}

// TimelineStats is what the ring can say about itself.
type TimelineStats struct {
	Frames  int       `json:"frames"`
	Dropped int       `json:"dropped"`
	Since   time.Time `json:"since"`
	Window  string    `json:"window"`
	Cap     int       `json:"cap"`
}

// Stats reports the ring's own state, including what it dropped.
//
// Dropped is on the page rather than in a log: a chart that silently loses its
// oldest frames reads as a quiet period that never happened.
func (t *Timeline) Stats() TimelineStats {
	if t == nil {
		return TimelineStats{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return TimelineStats{
		Frames:  t.size,
		Dropped: t.dropped,
		Since:   t.started,
		Window:  timelineWindow.String(),
		Cap:     len(t.frames),
	}
}
