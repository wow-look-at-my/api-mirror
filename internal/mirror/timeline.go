package mirror

import (
	"sync"
	"time"
)

// The window is what an operator looks at; the count is the memory ceiling
// that holds whatever the traffic does inside it.
const (
	timelineWindow = 24 * time.Hour
	timelineMax    = 100000
)

// Frame is timed event on the chart: what the mirror exchanged, when, and
// what it cost.
type Frame struct {
	// Seq orders frames by arrival, so a poll asks only for what is new.
	Seq       uint64        `json:"seq"`
	Lane      Lane          `json:"lane"`
	Group     string        `json:"group,omitempty"`
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

// Timeline is a bounded in-memory ring of everything the mirror exchanged.
type Timeline struct {
	mu      sync.Mutex
	frames  []Frame
	head    int
	size    int
	dropped int
	seq     uint64
	started time.Time
	now     func() time.Time
}

// NewTimeline returns an empty ring, in memory: a live view, not an audit log.
// It resets on restart, and the page says so.
func NewTimeline() *Timeline {
	return &Timeline{
		frames:  make([]Frame, timelineMax),
		started: time.Now(),
		now:     time.Now,
	}
}

// Observe records exchange. It never blocks on anything but its own lock,
// because the caller is on the path of the request being measured.
func (t *Timeline) Observe(e Exchange) {
	if t == nil {
		return
	}
	f := Frame{
		Lane:      e.Lane,
		Group:     e.Group,
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
	t.seq++
	f.Seq = t.seq
	if t.size == len(t.frames) {
		t.dropped++
		t.frames[t.head] = f
		t.head = (t.head + 1) % len(t.frames)
		return
	}
	t.frames[(t.head+t.size)%len(t.frames)] = f
	t.size++
}

// Frames returns everything still inside the window, oldest earliest.
func (t *Timeline) Frames() []Frame { return t.Since(0) }

// Since returns the frames inside the window that arrived after seq, oldest
// earliest.
//
// Eviction is lazy, here, rather than on a timer: a background goroutine that
// only ever deletes is a moving part with nothing to gain from moving. Only
// the expired run at the front is evicted.
func (t *Timeline) Since(seq uint64) []Frame {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-timelineWindow)
	for t.size > 0 && t.frames[t.head].At.Before(cutoff) {
		t.head = (t.head + 1) % len(t.frames)
		t.size--
	}
	out := make([]Frame, 0)
	for i := range t.size {
		f := t.frames[(t.head+i)%len(t.frames)]
		if f.Seq <= seq || f.At.Before(cutoff) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// TimelineStats is what the ring can say about itself.
type TimelineStats struct {
	Frames  int       `json:"frames"`
	Dropped int       `json:"dropped"`
	Since   time.Time `json:"since"`
	Window  string    `json:"window"`
	Cap     int       `json:"cap"`
	// Seq is the newest frame's sequence, the cursor for the next poll.
	Seq uint64 `json:"seq"`
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
		Seq:     t.seq,
	}
}
