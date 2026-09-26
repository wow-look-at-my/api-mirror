package mirror

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
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
	declared := set.New[string]()
	for _, ev := range spec.Events.List {
		declared.Add(ev.Type)
	}
	for _, typ := range githubStateMirrorTypes {
		assert.Truef(t, declared.Contains(typ), "the GitHub spec declares no event for %q", typ)
	}
}
