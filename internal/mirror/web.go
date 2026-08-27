package mirror

import (
	"embed"
	"net/http"
	"strconv"
	"strings"
)

// The dashboard's own files, compiled into the binary.
//
// They are plain HTML, CSS and ES modules with no build step. A toolchain
// between the source in this repo and the bytes that ship is one more thing
// that can be stale in a way nothing checks, and this page is small enough not
// to need one.

//go:embed web/index.html web/style.css web/app.js
var webFS embed.FS

// page serves the dashboard shell.
func (a *Admin) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != a.prefix+"/" {
		// The mux's trailing-slash pattern catches every path under the prefix.
		// Anything not named is a mistake, and answering the page for it would
		// make a typo look like a working URL.
		http.NotFound(w, r)
		return
	}
	body, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page fetches its own JSON with the same token it was opened with, so
	// it must not be cached anywhere but this browser tab.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// asset serves one of the page's own files.
func (a *Admin) asset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := webFS.ReadFile("web/" + name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "no-store")
		w.Write(body)
	}
}

// assetNames is what the page loads. The test that walks these is what keeps a
// renamed file from shipping as a blank dashboard: an embed that no longer
// matches is a compile error, but a <script src> that no longer matches is a
// page that loads and does nothing.
func assetNames() []string {
	names := []string{"index.html", "style.css", "app.js"}
	return names
}

// referencedAssets reads the file names index.html actually asks for.
func referencedAssets() ([]string, error) {
	body, err := webFS.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, marker := range []string{`href="`, `src="`} {
		rest := string(body)
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			name := rest[:j]
			if !strings.Contains(name, "://") {
				out = append(out, name)
			}
			rest = rest[j:]
		}
	}
	return out, nil
}
