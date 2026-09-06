package mirror

import (
	"net/http"
	"time"
)

// recorder wraps inbound response so the log records what the caller
// received rather than what the handler meant to send.
type recorder struct {
	http.ResponseWriter
	started time.Time
	method  string
	path    string

	status int
	bytes  int
	wrote  bool

	disposition Disposition
	shape       string
	resource    string
	reason      string
	principal   string
}

func newRecorder(w http.ResponseWriter, r *http.Request) *recorder {
	return &recorder{
		ResponseWriter: w,
		started:        time.Now(),
		method:         r.Method,
		path:           r.URL.Path,
		status:         http.StatusOK,
		// Unnamed is an error: the default that shows up and gets fixed.
		disposition: DispError,
	}
}

func (rec *recorder) WriteHeader(status int) {
	if rec.wrote {
		return
	}
	rec.wrote = true
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(p []byte) (int, error) {
	rec.wrote = true
	n, err := rec.ResponseWriter.Write(p)
	rec.bytes += n
	return n, err
}

// Flush keeps a streaming handler streaming: swallowing it would turn a live
// NDJSON feed into buffered dump at the end.
func (rec *recorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap is how ResponseController reaches deadlines and hijacking underneath.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// note records what this request was, from the handler that knows.
func (rec *recorder) note(d Disposition, shape, resource, reason string) {
	rec.disposition = d
	if shape != "" {
		rec.shape = shape
	}
	if resource != "" {
		rec.resource = resource
	}
	if reason != "" {
		rec.reason = reason
	}
}

// entry is the log line for this request.
func (rec *recorder) entry() Request {
	shape := rec.shape
	if shape == "" {
		shape = rec.path
	}
	return Request{
		At:          rec.started,
		Method:      rec.method,
		Path:        rec.path,
		Shape:       shape,
		Resource:    rec.resource,
		Disposition: rec.disposition,
		Reason:      rec.reason,
		Status:      rec.status,
		Bytes:       rec.bytes,
		Duration:    time.Since(rec.started),
		Principal:   rec.principal,
	}
}
