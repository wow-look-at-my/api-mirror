// compress.go holds gzipWriter.

package mirror

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// gzipWriter compresses what a handler writes. It is for the dashboard's own
// buffered JSON only.
type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

func (g *gzipWriter) WriteHeader(status int) {
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(status)
}

// acceptsGzip reports whether a request names gzip in Accept-Encoding.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}

// compressed runs a handler with its answer gzipped when the caller accepts it.
func compressed(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) {
	if !acceptsGzip(r) {
		serve(w, r)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	gz := gzip.NewWriter(w)
	defer gz.Close()
	serve(&gzipWriter{ResponseWriter: w, gz: gz}, r)
}
