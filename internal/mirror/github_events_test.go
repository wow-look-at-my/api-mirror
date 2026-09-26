package mirror

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// githubStateMirrorTypes are the webhook types github-state-mirror acted on.
// The GitHub spec replaces it, so each needs a declared event here. A type
// the spec drops is ignored at ingest, and the resources it moved then wait
// out their TTL with nothing to report it.
var githubStateMirrorTypes = []string{
	"check_run", "check_suite", "create", "delete", "installation", "installation_repositories",
	"label", "membership", "organization", "pull_request", "pull_request_review", "push",
	"repository", "status", "workflow_job", "workflow_run",
}

func TestGitHubSpecDeclaresEveryTypeGithubStateMirrorHandled(t *testing.T) {
	spec, err := Load(filepath.Join(repoRoot, "samples", "github", "github.xml"))
	require.NoError(t, err)
	declared := map[string]struct{}{}
	for _, ev := range spec.Events.List {
		declared[ev.Type] = struct{}{}
	}
	for _, typ := range githubStateMirrorTypes {
		_, ok := declared[typ]
		assert.Truef(t, ok, "the GitHub spec declares no event for %q", typ)
	}
}
