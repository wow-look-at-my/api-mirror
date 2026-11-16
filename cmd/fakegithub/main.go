// Command fakegithub is a deterministic GitHub REST stand-in for tests. It
// answers /user, the repo, pulls, refs, statuses and the list routes in
// GitHub's own document shapes — fields the mirror does not keep included. A
// test can compare the mirror's answer with the upstream's — and counts every
// request so a test can prove a cache hit made none.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const api = "https://api.github.com"

var (
	repoPath      = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)$`)
	statusesPath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/(?:commits/([^/]+)/statuses|statuses/([^/]+))$`)
	pullsPath     = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`)
	pullPath      = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/([0-9]+)$`)
	branchRefPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/ref/heads/([^/]+)$`)
	gitCommitPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/commits/([^/]+)$`)
	commitsPath   = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits$`)
	branchesPath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/branches$`)
	runPath       = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/runs/([0-9]+)$`)
	runJobsPath   = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/runs/([0-9]+)/jobs$`)
	workflowsPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/workflows$`)
	workflowPath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/workflows/([^/]+)$`)
	tagsPath      = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/tags$`)
	whereHeadPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([^/]+)/branches-where-head$`)
	commitPath    = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([^/]+)$`)
	annotatePath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/check-runs/([0-9]+)/annotations$`)
	rulesPath     = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/rules/branches/(.+)$`)
	contentsPath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/contents/(.+)$`)
)

type doc = map[string]any

func user(login string) doc {
	return doc{
		"login": login, "id": 9, "node_id": "U_kgDO", "type": "User", "site_admin": false,
		"url": api + "/users/" + login, "html_url": "https://github.com/" + login,
		"avatar_url": "https://avatars.githubusercontent.com/u/9",
	}
}

func repo(owner, name string) doc {
	full := owner + "/" + name
	return doc{
		"id": 1, "node_id": "R_kgDO", "name": name, "full_name": full,
		"owner": user(owner), "private": false, "visibility": "public",
		"description": nil, "fork": false, "default_branch": "main",
		"pushed_at": "2026-08-25T00:00:00Z", "archived": false, "topics": []string{"fake"},
		"stargazers_count": 3, "url": api + "/repos/" + full, "html_url": "https://github.com/" + full,
	}
}

// pull is one pull request. GitHub's list answer leaves out the merge fields
// that only the answer carries.
func pull(owner, name string, number int, single bool) doc {
	full := owner + "/" + name
	p := doc{
		"id": 100 + number, "node_id": "PR_kwDO", "number": number, "state": "open",
		"title": "Change " + strconv.Itoa(number), "draft": false, "user": user("octocat"),
		"head":             doc{"label": "octocat:feature", "ref": "feature", "sha": "aaaa000000000000000000000000000000000001", "repo": repo(owner, name)},
		"base":             doc{"label": owner + ":main", "ref": "main", "sha": "bbbb000000000000000000000000000000000002", "repo": repo(owner, name)},
		"merge_commit_sha": "cccc000000000000000000000000000000000003",
		"updated_at":       "2026-08-25T10:00:00Z",
		"url":              api + "/repos/" + full + "/pulls/" + strconv.Itoa(number),
		"html_url":         "https://github.com/" + full + "/pull/" + strconv.Itoa(number),
	}
	if single {
		p["merged"] = false
		p["mergeable"] = true
		p["commits"] = 1
	}
	return p
}

func status(owner, name, sha string, id int, state string, description, target any, created string) doc {
	return doc{
		"id": id, "node_id": "SC_kwDO", "context": "ci/build", "state": state,
		"description": description, "target_url": target, "created_at": created, "updated_at": created,
		"creator":    user("ci-bot"),
		"url":        api + "/repos/" + owner + "/" + name + "/statuses/" + sha,
		"avatar_url": "https://avatars.githubusercontent.com/u/1",
	}
}

func gitCommit(owner, name, sha string) doc {
	base := api + "/repos/" + owner + "/" + name + "/git"
	who := doc{"name": "Octo Cat", "email": "octo@example.com", "date": "2026-08-25T09:00:00Z"}
	return doc{
		"sha": sha, "node_id": "C_kwDO", "url": base + "/commits/" + sha,
		"html_url": "https://github.com/" + owner + "/" + name + "/commit/" + sha,
		"author":   who, "committer": who, "message": "Fix the thing",
		"tree":         doc{"sha": "dddd000000000000000000000000000000000004", "url": base + "/trees/dddd"},
		"parents":      []doc{{"sha": "eeee000000000000000000000000000000000005", "url": base + "/commits/eeee"}},
		"verification": doc{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil},
	}
}

func answer(path string) (any, bool) {
	if path == "/user" {
		return doc{"id": 42, "login": "fake-user"}, true
	}
	if m := repoPath.FindStringSubmatch(path); m != nil {
		return repo(m[1], m[2]), true
	}
	if m := statusesPath.FindStringSubmatch(path); m != nil {
		sha := m[3] + m[4]
		return []doc{
			status(m[1], m[2], sha, 1, "pending", nil, nil, "2026-08-25T11:00:00Z"),
			status(m[1], m[2], sha, 2, "success", "2/2 passed", "https://ci.example.com/2", "2026-08-25T12:00:00Z"),
		}, true
	}
	if m := pullsPath.FindStringSubmatch(path); m != nil {
		return []doc{pull(m[1], m[2], 1, false), pull(m[1], m[2], 2, false)}, true
	}
	if m := pullPath.FindStringSubmatch(path); m != nil {
		n, _ := strconv.Atoi(m[3])
		return pull(m[1], m[2], n, true), true
	}
	if m := branchRefPath.FindStringSubmatch(path); m != nil {
		base := api + "/repos/" + m[1] + "/" + m[2] + "/git"
		return doc{
			"ref": "refs/heads/" + m[3], "node_id": "REF_kwDO", "url": base + "/refs/heads/" + m[3],
			"object": doc{"sha": "ffff000000000000000000000000000000000006", "type": "commit", "url": base + "/commits/ffff"},
		}, true
	}
	if m := gitCommitPath.FindStringSubmatch(path); m != nil {
		return gitCommit(m[1], m[2], m[3]), true
	}
	if m := commitsPath.FindStringSubmatch(path); m != nil {
		full := m[1] + "/" + m[2]
		return []doc{{
			"sha": "abc123", "node_id": "C_kwDO", "commit": gitCommit(m[1], m[2], "abc123"),
			"author": user("octocat"), "committer": user("octocat"),
			"url": api + "/repos/" + full + "/commits/abc123", "html_url": "https://github.com/" + full + "/commit/abc123",
		}}, true
	}
	if v, ok := actionsAnswer(path); ok {
		return v, true
	}
	if v, ok := commitAnswer(path); ok {
		return v, true
	}
	if m := branchesPath.FindStringSubmatch(path); m != nil {
		return []doc{
			{"name": "feature", "protected": false, "commit": doc{"sha": "aaaa000000000000000000000000000000000001", "url": api + "/repos/" + m[1] + "/" + m[2] + "/commits/aaaa"}},
			{"name": "main", "protected": true, "commit": doc{"sha": "ffff000000000000000000000000000000000006", "url": api + "/repos/" + m[1] + "/" + m[2] + "/commits/ffff"}},
			{"name": "dev", "protected": false, "commit": doc{"sha": "3333333333333333333333333333333333333333", "url": api + "/repos/" + m[1] + "/" + m[2] + "/commits/3333"}},
		}, true
	}
	return nil, false
}

func main() {
	listen := flag.String("listen", ":8081", "listen address")
	appID := flag.String("app-id", "", "play a GitHub App with this id; needs -app-key")
	appKey := flag.String("app-key", "", "write the App's new private key to this file")
	flag.Parse()

	var app *fakeApp
	if *appID != "" {
		var err error
		if app, err = newFakeApp(*appID, *appKey); err != nil {
			log.Fatalf("fakegithub: %v", err)
		}
	}

	var requests atomic.Int64
	var mu sync.Mutex
	var served []string
	var repoAuth atomic.Value
	repoAuth.Store("")

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
	// The Authorization the latest /repos/ request carried, so a test can see who asked.
	mux.HandleFunc("/_auth", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, repoAuth.Load())
	})
	mux.HandleFunc("/_exchanges", func(w http.ResponseWriter, r *http.Request) {
		if app == nil {
			http.Error(w, "not playing an App: start with -app-id", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "%d", app.count())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		served = append(served, r.Method+":"+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			repoAuth.Store(r.Header.Get("Authorization"))
		}
		if app != nil && app.answer(w, r) {
			return
		}
		if raw(w, r) {
			return
		}
		if v, ok := answer(r.URL.Path); ok {
			_ = json.NewEncoder(w).Encode(v)
			return
		}

		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	})

	log.Printf("fakegithub: serving on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
