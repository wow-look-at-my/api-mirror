// Command fakegithub is a deterministic GitHub REST stand-in for tests.
// It answers /user, the repo, commit-statuses and branches routes with fixed, request-derived
// JSON, and counts every request so a test can prove a cache hit made none.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var repoPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)$`)
var statusesPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([^/]+)/statuses$`)
var branchesPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/branches$`)

func main() {
	listen := flag.String("listen", ":8081", "listen address")
	flag.Parse()

	var requests atomic.Int64
	var mu sync.Mutex
	var served []string

	mux := http.NewServeMux()
	mux.HandleFunc("/_requests", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%d", requests.Load())
	})
	// /_log names every request served, so a failing count says which call it was.
	mux.HandleFunc("/_log", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprint(w, strings.Join(served, " "))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		served = append(served, r.Method+":"+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": "fake-user"})
			return
		}

		if m := repoPath.FindStringSubmatch(r.URL.Path); m != nil {
			owner, name := m[1], m[2]
			json.NewEncoder(w).Encode(map[string]any{
				"id":             1,
				"name":           name,
				"full_name":      owner + "/" + name,
				"owner":          map[string]any{"login": owner},
				"visibility":     "public",
				"private":        false,
				"default_branch": "main",
				"pushed_at":      "2026-08-25T00:00:00Z",
				"archived":       false,
				"topics":         []string{"fake"},
			})
			return
		}

		if m := statusesPath.FindStringSubmatch(r.URL.Path); m != nil {
			sha := m[3]
			_ = sha // the route key supplies sha; a per-item field would shadow it (absorb has no from= override here)
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "context": "ci/build", "state": "pending", "description": nil, "target_url": nil, "created_at": "2026-08-25T11:00:00Z"},
				{"id": 2, "context": "ci/build", "state": "success", "description": "2/2 passed", "target_url": "https://ci.example.com/2", "created_at": "2026-08-25T12:00:00Z"},
			})
			return
		}

		if branchesPath.MatchString(r.URL.Path) {
			json.NewEncoder(w).Encode([]map[string]any{
				{"name": "main", "commit": map[string]any{"sha": "1111111111111111111111111111111111111111"}, "protected": true},
				{"name": "dev", "commit": map[string]any{"sha": "3333333333333333333333333333333333333333"}, "protected": false},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	})

	log.Printf("fakegithub: serving on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
