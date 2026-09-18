package mirror

import (
	"compress/gzip"
	"net/http"
	"os"
	"strings"
)

// gzipWriter compresses an admin answer. The data plane is never wrapped: its
// header contract is pinned, and a streamed check must keep flushing.
type gzipWriter struct {
	http.ResponseWriter
	zw *gzip.Writer
}

func (g *gzipWriter) WriteHeader(status int) {
	g.Header().Del("Content-Length")
	g.Header().Set("Content-Encoding", "gzip")
	g.Header().Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if g.Header().Get("Content-Encoding") == "" {
		g.WriteHeader(http.StatusOK)
	}
	return g.zw.Write(b)
}

// compressible reports whether an admin request's answer is buffered JSON a
// browser asked to have compressed.
func compressible(r *http.Request, prefix string) bool {
	return strings.HasPrefix(r.URL.Path, prefix+"/api/") &&
		r.URL.Query().Get("stream") != "1" &&
		strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// fileSize is a file's size, or empty when it does not exist yet.
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// JobsView is every background job's standing, JSON only.
type JobsView struct {
	Refresh  RefreshStats `json:"refresh"`
	Replay   ReplayStats  `json:"replay"`
	Notify   NotifyStats  `json:"notify"`
	InFlight int64        `json:"fetches_in_flight"`
}

func (a *Admin) jobs(w http.ResponseWriter, r *http.Request) {
	e := a.engine
	writeJSON(w, http.StatusOK, JobsView{
		Refresh:  e.refresh.Stats(),
		Replay:   e.replay.Stats(),
		Notify:   e.notify.Stats(r.Context()),
		InFlight: e.fresh.inflight.Load(),
	})
}
