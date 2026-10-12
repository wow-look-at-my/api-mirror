package mirror

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The GitHub sample's deliveries, as GitHub shapes them. A when= that does
// not render. Otherwise, a clock a view does not carry, fails the whole
// delivery at run time, and nothing at load time can see it.
func TestGitHubSpecAppliesTheDeliveriesItDeclares(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", testSecret)
	t.Setenv("WEBHOOK_REORDER_WINDOW", "0")
	spec, err := Load(filepath.Join(repoRoot, "samples", "github", "github.xml"))
	require.NoError(t, err)
	store := openStore(t, dbPath(t), spec)
	in, err := NewIngest(spec, store, map[string]any{})
	require.NoError(t, err)

	repo := map[string]any{
		"name": "tools", "full_name": "acme/tools", "owner": map[string]any{"login": "acme"},
		"pushed_at": "2026-10-12T04:00:00Z", "updated_at": "2026-10-12T04:00:00Z", "default_branch": "main",
	}
	at := "2026-10-12T04:00:00Z"
	cases := []struct {
		typ     string
		payload map[string]any
	}{
		{"push", map[string]any{"ref": "refs/tags/v1", "after": "abc", "repository": repo}},
		{"push", map[string]any{"ref": "refs/heads/main", "after": "abc", "repository": repo}},
		{"create", map[string]any{"ref": "v1", "ref_type": "tag", "repository": repo}},
		{"delete", map[string]any{"ref": "topic", "ref_type": "branch", "repository": repo}},
		{"repository_ruleset", map[string]any{"action": "edited", "repository_ruleset": map[string]any{"id": 1, "updated_at": at},
			"repository": repo, "organization": map[string]any{"login": "acme"}}},
		{"repository_ruleset", map[string]any{"action": "created", "repository_ruleset": map[string]any{"id": 2, "updated_at": at},
			"organization": map[string]any{"login": "acme"}}},
		{"workflow_run", map[string]any{"action": "in_progress", "repository": repo,
			"workflow_run": map[string]any{"id": 5, "status": "in_progress", "updated_at": at, "pull_requests": []any{}}}},
		{"check_run", map[string]any{"action": "created", "repository": repo,
			"check_run": map[string]any{"id": 8, "head_sha": "abc", "started_at": at, "completed_at": nil}}},
		{"check_run", map[string]any{"action": "completed", "repository": repo,
			"check_run": map[string]any{"id": 8, "head_sha": "abc", "started_at": at, "completed_at": at}}},
		{"workflow_job", map[string]any{"action": "queued", "repository": repo,
			"workflow_job": map[string]any{"id": 6, "run_id": 5, "started_at": at, "completed_at": nil}}},
		{"pull_request", map[string]any{"action": "synchronize", "number": 7, "repository": repo,
			"pull_request": map[string]any{"number": 7, "updated_at": at, "state": "open",
				"head": map[string]any{"sha": "abc", "ref": "topic"}, "base": map[string]any{"sha": "def", "ref": "main"}}}},
	}
	for _, c := range cases {
		w := deliverGitHub(t, in, c.typ, c.payload)
		assert.NotEqual(t, http.StatusInternalServerError, w.Code, "%s %v: %s", c.typ, c.payload["action"], w.Body.String())
	}

	res, ok := store.Resource("workflow_run")
	require.True(t, ok)
	row, err := store.Get(t.Context(), res, map[string]string{"owner": "acme", "repo": "tools", "run_id": "5"})
	require.NoError(t, err)
	require.NotNil(t, row, "the workflow_run delivery writes the run document")
	assert.Contains(t, row["document"], `"status":"in_progress"`)
}

func deliverGitHub(t *testing.T, in *Ingest, typ string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body := deliveryBody(t, payload)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", typ)
	req.Header.Set("X-Hub-Signature-256", signBody(testSecret, body))
	w := httptest.NewRecorder()
	in.ServeHTTP(w, req)
	return w
}
