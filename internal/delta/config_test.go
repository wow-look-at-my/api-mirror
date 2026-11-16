package delta

import _ "embed"

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "delta.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("DELTA_TEST_TOKEN", "secret")
	c, err := LoadConfig(writeConfig(t, testLoadConfigYml))
	require.NoError(t, err)
	require.NoError(t, c.Validate())
	assert.Equal(t, "https://api.example", c.Truth.base)
	assert.Equal(t, "token secret", c.Truth.Headers["Authorization"])
	assert.Equal(t, Duration(5*time.Second), c.Timeout)
	assert.Equal(t, TruthName, c.Serve)
	assert.Equal(t, []string{"Content-Type", "Link", "Location"}, c.Headers)
	assert.True(t, c.compares("GET", "/x"))
	assert.False(t, c.compares("POST", "/x"))

	missingURL := Diff{Kind: KindMissing, loc: loc("items", 0, "url")}
	assert.Equal(t, 0, c.ruleFor("GET", "/x", "local", missingURL))
	assert.Equal(t, -1, c.ruleFor("GET", "/x", "local", Diff{Kind: KindChanged, loc: loc("url")}))
	assert.Equal(t, 1, c.ruleFor("GET", "/x", "local", Diff{Kind: KindHeader, Path: "Link"}))
	assert.Equal(t, 2, c.ruleFor("GET", "/rate_limit", "local", Diff{Kind: KindChanged, loc: loc("rate")}))
	assert.Equal(t, -1, c.ruleFor("GET", "/rate_limit/x", "local", Diff{Kind: KindChanged, loc: loc("rate")}))
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, testLoadConfigRejectsUnknownKeysYml))
	assert.ErrorContains(t, err, "ignroe")
}

func TestValidateReportsEveryProblem(t *testing.T) {
	c := &Config{
		Truth:   Target{URL: "not a url"},
		Mirrors: []Target{{URL: "http://a"}, {Name: "m", URL: "http://b", Headers: map[string]string{"X": "${DELTA_UNSET_VAR}"}}},
		Serve:   "nope",
		Ignore: []*Rule{
			{Reason: "names nothing"},
			{Path: "$.a[?(@)]"},
			{Path: "$.a", Header: "Link"},
			{Path: "$.a", Kinds: []Kind{KindStatus}},
			{Kinds: []Kind{"bogus"}},
			{Kinds: []Kind{KindStatus}, Mirror: "ghost"},
			{Kinds: []Kind{KindStatus}, Match: Match{Route: "("}},
		},
	}
	err := c.Validate()
	require.Error(t, err)
	for _, want := range []string{
		"truth: url", "mirrors[0]: name is required", "DELTA_UNSET_VAR", `serve: "nope"`,
		"ignore[0]: a rule must name", "ignore[1]", "ignore[2]: path and header", "ignore[3]: path selects JSON",
		`ignore[4]: kind "bogus"`, `ignore[5]: mirror "ghost"`, "ignore[6]: route",
	} {
		assert.ErrorContains(t, err, want)
	}
}

func TestRuleScopesToMirror(t *testing.T) {
	c := &Config{
		Truth:   Target{URL: "http://t"},
		Mirrors: []Target{{Name: "a", URL: "http://a"}, {Name: "b", URL: "http://b"}},
		Ignore:  []*Rule{{Kinds: []Kind{KindStatus}, Mirror: "a"}},
	}
	require.NoError(t, c.Validate())
	d := Diff{Kind: KindStatus}
	assert.Equal(t, 0, c.ruleFor("GET", "/", "a", d))
	assert.Equal(t, -1, c.ruleFor("GET", "/", "b", d))
}

//go:embed testdata/test-load-config.yml
var testLoadConfigYml string

//go:embed testdata/test-load-config-rejects-unknown-keys.yml
var testLoadConfigRejectsUnknownKeysYml string
