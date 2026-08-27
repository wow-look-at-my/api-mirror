package mirror

import (
	"embed"
	"net/http"
	"strconv"
	"strings"
)

//go:embed web/index.html web/style.css web/app.js
var webFS embed.FS

// page serves the dashboard shell.
//
// The page is plain HTML, CSS and ES modules with no build step. A toolchain
// between the source in this repo and the bytes that ship is one more thing
// that can be stale in a way nothing checks.
func (a *Admin) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != a.prefix+"/" {
		// Serving the page here would make a typo look like a working URL.
		http.NotFound(w, r)
		return
	}
	body, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// It carries the token it was opened with: cache it in this tab and nowhere.
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

// assetNames is what the page loads; a test walks it against index.html.
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
			// A URL and an inline data: URI are not files this ships.
			name := rest[:j]
			if !strings.Contains(name, "://") && !strings.HasPrefix(name, "data:") {
				out = append(out, name)
			}
			rest = rest[j:]
		}
	}
	return out, nil
}
