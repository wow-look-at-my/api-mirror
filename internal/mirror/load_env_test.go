package mirror

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadTimeValuesReadTheEnvironment(t *testing.T) {
	t.Setenv("MIRROR_TEST_REFRESH", "90m")
	t.Setenv("MIRROR_TEST_ORIGINS", "https://a.example, https://b.example")
	spec := opsSpec(t, `	<refresh interval='{{ .env.MIRROR_TEST_REFRESH | default "6h" }}'/>
	<cors><origin>{{ .env.MIRROR_TEST_ORIGINS | default "*" }}</origin></cors>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, 90*time.Minute, spec.Refresh.Interval)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, spec.CORS.Origins,
		"an environment variable carries a list with commas")
}

func TestLoadTimeValueDefaultsWhenTheVariableIsUnset(t *testing.T) {
	spec := opsSpec(t, `	<refresh interval='{{ .env.MIRROR_TEST_UNSET_VAR | default "6h" }}'/>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, 6*time.Hour, spec.Refresh.Interval)
}

func TestZeroTurnsTheReplayerAndTheDebounceOff(t *testing.T) {
	spec := opsSpec(t, `	<replay interval="0"><list>/d</list><redeliver>/d/x</redeliver><id>id</id></replay>`)
	require.NoError(t, spec.validate(), "0 is an operator's decision to turn recovery off, not a typo")
	assert.True(t, spec.Replay.Off)

	up, err := buildUpstream(mustNode(t, `<upstream base="https://x.example" debounce="0"/>`))
	require.NoError(t, err)
	assert.Zero(t, up.Debounce)
}

func TestAnUnparseableRenderedDurationFailsTheLoad(t *testing.T) {
	t.Setenv("MIRROR_TEST_REFRESH", "soon")
	_, err := ParseSpec([]byte(`<mirror name="m"><refresh interval='{{ .env.MIRROR_TEST_REFRESH }}'/></mirror>`))
	require.Error(t, err, "a typo in an environment variable must fail at boot, not revert quietly")
}

func mustNode(t *testing.T, src string) *node {
	t.Helper()
	n, err := parseDOM([]byte(src))
	require.NoError(t, err)
	return n
}
