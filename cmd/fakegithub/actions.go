package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Answers for Actions, commits and the routes whose answer is not JSON.

var (
	runLogsPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/runs/([0-9]+)/logs$`)
	jobLogsPath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/jobs/([0-9]+)/logs$`)
	archivePath = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/(tarball|zipball)/(.+)$`)
	readmePath  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/readme$`)
)

func workflowRun(owner, name string, id int) doc {
	full := owner + "/" + name
	return doc{
		"id": id, "node_id": "WFR_kwLO", "name": "CI", "head_branch": "main",
		"head_sha": "ffff000000000000000000000000000000000006", "path": ".github/workflows/ci.yml",
		"display_title": "Fix the thing", "run_number": 12, "event": "push", "status": "completed",
		"conclusion": "success", "workflow_id": 3, "check_suite_id": 40, "run_attempt": 1,
		"pull_requests":  []doc{{"number": 7, "id": 107, "head": doc{"ref": "feature", "sha": "aaaa000000000000000000000000000000000001"}}},
		"created_at":     "2026-08-25T09:00:00Z", "updated_at": "2026-08-25T09:05:00Z",
		"actor":          user("octocat"), "repository": repo(owner, name), "head_repository": repo(owner, name),
		"head_commit":    doc{"id": "ffff000000000000000000000000000000000006", "message": "Fix the thing", "timestamp": "2026-08-25T09:00:00Z"},
		"url":            api + "/repos/" + full + "/actions/runs/" + strconv.Itoa(id),
		"jobs_url":       api + "/repos/" + full + "/actions/runs/" + strconv.Itoa(id) + "/jobs",
		"logs_url":       api + "/repos/" + full + "/actions/runs/" + strconv.Itoa(id) + "/logs",
		"html_url":       "https://github.com/" + full + "/actions/runs/" + strconv.Itoa(id),
		"referenced_workflows": []doc{},
	}
}

func workflow(owner, name, id string) doc {
	full := owner + "/" + name
	return doc{
		"id": 3, "node_id": "W_kwDO", "name": "CI", "path": ".github/workflows/ci.yml", "state": "active",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		"url":       api + "/repos/" + full + "/actions/workflows/" + id,
		"html_url":  "https://github.com/" + full + "/blob/main/.github/workflows/ci.yml",
		"badge_url": "https://github.com/" + full + "/workflows/CI/badge.svg",
	}
}

func actionsAnswer(path string) (any, bool) {
	if m := runPath.FindStringSubmatch(path); m != nil {
		id, _ := strconv.Atoi(m[3])
		return workflowRun(m[1], m[2], id), true
	}
	if m := runJobsPath.FindStringSubmatch(path); m != nil {
		id, _ := strconv.Atoi(m[3])
		return doc{"total_count": 1, "jobs": []doc{{
			"id": 6, "run_id": id, "name": "test", "status": "completed", "conclusion": "success",
			"head_sha": "ffff000000000000000000000000000000000006", "started_at": "2026-08-25T09:01:00Z",
			"steps": []doc{{"name": "Run tests", "status": "completed", "conclusion": "success", "number": 1}},
		}}}, true
	}
	if m := workflowsPath.FindStringSubmatch(path); m != nil {
		return doc{"total_count": 1, "workflows": []doc{workflow(m[1], m[2], "3")}}, true
	}
	if m := workflowPath.FindStringSubmatch(path); m != nil {
		return workflow(m[1], m[2], m[3]), true
	}
	if m := annotatePath.FindStringSubmatch(path); m != nil {
		return []doc{{
			"path": "main.go", "start_line": 3, "end_line": 3, "start_column": nil, "end_column": nil,
			"annotation_level": "warning", "title": "vet", "message": "unused variable", "raw_details": "",
			"blob_href": "https://github.com/" + m[1] + "/" + m[2] + "/blob/ffff/main.go",
		}}, true
	}
	return nil, false
}

func commitAnswer(path string) (any, bool) {
	if m := tagsPath.FindStringSubmatch(path); m != nil {
		full := m[1] + "/" + m[2]
		return []doc{{
			"name": "v1.0.0", "node_id": "REF_kwDO",
			"commit":      doc{"sha": "ffff000000000000000000000000000000000006", "url": api + "/repos/" + full + "/commits/ffff"},
			"zipball_url": api + "/repos/" + full + "/zipball/refs/tags/v1.0.0",
			"tarball_url": api + "/repos/" + full + "/tarball/refs/tags/v1.0.0",
		}}, true
	}
	if m := whereHeadPath.FindStringSubmatch(path); m != nil {
		return []doc{{"name": "main", "protected": true, "commit": doc{"sha": m[3], "url": api + "/repos/" + m[1] + "/" + m[2] + "/commits/" + m[3]}}}, true
	}
	if m := commitPath.FindStringSubmatch(path); m != nil {
		full := m[1] + "/" + m[2]
		return doc{
			"sha": m[3], "node_id": "C_kwDO", "commit": gitCommit(m[1], m[2], m[3]),
			"author": nil, "committer": nil,
			"parents": []doc{{"sha": "eeee000000000000000000000000000000000005", "url": api + "/repos/" + full + "/commits/eeee"}},
			"stats":   doc{"additions": 2, "deletions": 1, "total": 3},
			"files": []doc{{
				"sha": "1111000000000000000000000000000000000001", "filename": "README.md", "status": "modified",
				"additions": 2, "deletions": 1, "changes": 3, "patch": "@@ -1 +1,2 @@\n-a\n+b\n+c",
			}},
			"url": api + "/repos/" + full + "/commits/" + m[3], "html_url": "https://github.com/" + full + "/commit/" + m[3],
		}, true
	}
	if m := rulesPath.FindStringSubmatch(path); m != nil {
		return []doc{{"type": "pull_request", "ruleset_source_type": "Organization", "ruleset_source": m[1], "ruleset_id": 18208166}}, true
	}
	if m := contentsPath.FindStringSubmatch(path); m != nil {
		full := m[1] + "/" + m[2]
		if m[3] == "sub" {
			return doc{
				"type": "submodule", "name": "sub", "path": "sub", "sha": "2222000000000000000000000000000000000002", "size": 0,
				"submodule_git_url": "https://github.com/acme/sub.git", "download_url": nil,
				"url":   api + "/repos/" + full + "/contents/sub",
				"_links": doc{"self": api + "/repos/" + full + "/contents/sub", "git": api + "/repos/acme/sub/git/trees/2222", "html": "https://github.com/acme/sub/tree/2222"},
			}, true
		}
		parts := strings.Split(m[3], "/")
		return doc{
			"type": "file", "name": parts[len(parts)-1], "path": m[3], "sha": "3333000000000000000000000000000000000003",
			"size": 5, "encoding": "base64", "content": "aGVsbG8=", "url": api + "/repos/" + full + "/contents/" + m[3],
		}, true
	}
	return nil, false
}

// raw answers the routes whose answer is not a JSON document: a redirect to a
// signed or unsigned URL, a diff, and a legal block. It reports whether it
// answered.
func raw(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	signed := func(kind, id string) string {
		return "https://results.example/logs/" + kind + "/" + id + "?se=" +
			url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)) + "&sig=fake"
	}
	switch {
	case runLogsPath.MatchString(path):
		redirect(w, signed("run", runLogsPath.FindStringSubmatch(path)[3]))
	case jobLogsPath.MatchString(path):
		redirect(w, signed("job", jobLogsPath.FindStringSubmatch(path)[3]))
	case archivePath.MatchString(path):
		m := archivePath.FindStringSubmatch(path)
		ext := "tar.gz"
		if m[3] == "zipball" {
			ext = "zip"
		}
		redirect(w, "https://codeload.example/"+m[1]+"/"+m[2]+"/legacy."+ext+"/refs/tags/"+m[4])
	case pullPath.MatchString(path) && strings.Contains(r.Header.Get("Accept"), "diff"):
		w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
		_, _ = w.Write([]byte("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-a\n+b\n"))
	case readmePath.MatchString(path) && readmePath.FindStringSubmatch(path)[2] == "Blocked":
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		_ = json.NewEncoder(w).Encode(doc{
			"message": "Repository access blocked",
			"block":   doc{"reason": "dmca", "created_at": "2026-01-01T00:00:00Z", "html_url": "https://github.com/github/dmca/blob/master/x.md"},
		})
	default:
		return false
	}
	return true
}

func redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}
