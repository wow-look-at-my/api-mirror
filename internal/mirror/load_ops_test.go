package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operational half of the grammar. Each of these declarations turns a
// mechanism on, and each rejection here is a mistake that would otherwise be
// discovered from behaviour rather than from a load failure.

// opsSpec wraps ops declarations around a minimal working mirror.
func opsSpec(t *testing.T, body string) *Spec {
	t.Helper()
	src := `<mirror name="ops">
	<upstream base="https://api.example.com"/>
	<resource name="thing" ttl="1h">
		<key name="id" from="id"/>
		<field name="title" type="text">title</field>
		<reveal><public>true</public></reveal>
	</resource>
	<route method="GET" path="/things/{id}" resource="thing"/>
` + body + `
</mirror>`
	spec, err := ParseSpec([]byte(src))
	require.NoError(t, err)
	return spec
}

func TestLoadDashboardAndItsToken(t *testing.T) {
	t.Setenv("MIRROR_TOKEN", "from-the-environment")
	spec := opsSpec(t, `	<dashboard path="/_ops" title="Ops">
		<token><value name="env.MIRROR_TOKEN"/></token>
	</dashboard>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, "/_ops", spec.Dashboard.Path)
	assert.Equal(t, "Ops", spec.Dashboard.Title)

	store := openStore(t, dbPath(t), spec)
	e, err := NewEngine(spec, store, nil)
	require.NoError(t, err)
	assert.Equal(t, "from-the-environment", e.admin.token)
	assert.False(t, e.admin.Minted(), "a declared token is the operator's, not one we invent")
}

func TestLoadRateLimitHeadersAndDebounce(t *testing.T) {
	src := `<mirror name="ops">
	<upstream base="https://api.example.com" debounce="5s" retry-after="Retry-After">
		<ratelimit limit="X-RateLimit-Limit" remaining="X-RateLimit-Remaining"
			used="X-RateLimit-Used" reset="X-RateLimit-Reset" resource="X-RateLimit-Resource"/>
	</upstream>
	<resource name="thing" ttl="1h">
		<key name="id" from="id"/>
		<field name="title" type="text">title</field>
		<reveal><public>true</public></reveal>
	</resource>
	<route method="GET" path="/things/{id}" resource="thing"/>
</mirror>`
	spec, err := ParseSpec([]byte(src))
	require.NoError(t, err)
	require.NoError(t, spec.validate())

	assert.Equal(t, 5*time.Second, spec.Upstream.Debounce)
	assert.True(t, spec.Upstream.Rate.declared())
	assert.Equal(t, "X-RateLimit-Reset", spec.Upstream.Rate.Reset)
}

func TestDebounceLongerThanTheCapFailsAtLoad(t *testing.T) {
	src := strings.Replace(`<mirror name="ops">
	<upstream base="https://api.example.com" debounce="5m"/>
	<resource name="thing" ttl="1h">
		<key name="id" from="id"/>
		<field name="title" type="text">title</field>
		<reveal><public>true</public></reveal>
	</resource>
	<route method="GET" path="/things/{id}" resource="thing"/>
</mirror>`, "\n", "\n", 1)
	spec, err := ParseSpec([]byte(src))
	require.NoError(t, err)
	err = spec.validate()
	require.Error(t, err, "every uncacheable read waits this out, so a fat-fingered value must fail at boot")
	assert.Contains(t, err.Error(), "debounce")
}

func TestLoadCORSAlwaysExposesTheMirrorsOwnHeaders(t *testing.T) {
	spec := opsSpec(t, `	<cors max-age="10m">
		<origin>https://viewer.example</origin>
		<expose>X-Total-Count</expose>
	</cors>`)
	require.NoError(t, spec.validate())
	require.NotNil(t, spec.CORS)
	assert.Equal(t, []string{"https://viewer.example"}, spec.CORS.Origins)
	assert.Contains(t, spec.CORS.Expose, "X-Total-Count")
	assert.Contains(t, spec.CORS.Expose, "X-Mirror-Cache",
		"a spec author cannot be expected to know the engine's private vocabulary")
	assert.Equal(t, 10*time.Minute, spec.CORS.MaxAge)
}

func TestCORSWithNoOriginIsRejected(t *testing.T) {
	spec := opsSpec(t, `	<cors max-age="1m"/>`)
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin", "a policy that allows nothing while looking like a policy is worse than none")
}

func TestLoadRefreshAndItsKinds(t *testing.T) {
	spec := opsSpec(t, `	<refresh interval="6h">
		<kind>thing</kind>
	</refresh>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, 6*time.Hour, spec.Refresh.Interval)
	assert.Equal(t, []string{"thing"}, spec.Refresh.Kinds)
}

func TestRefreshNamingAKindNoRouteServesIsRejected(t *testing.T) {
	spec := opsSpec(t, `	<refresh interval="6h">
		<kind>nothing-serves-this</kind>
	</refresh>`)
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing-serves-this")
}

func TestLoadReplay(t *testing.T) {
	spec := opsSpec(t, `	<replay interval="5m" method="POST" lookback="24h" max="25">
		<list>/app/hook/deliveries?status=failure</list>
		<redeliver>/app/hook/deliveries/<value name="delivery.id"/>/attempts</redeliver>
		<id>id</id>
		<at>delivered_at</at>
	</replay>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, 5*time.Minute, spec.Replay.Interval)
	assert.Equal(t, 25, spec.Replay.Max)
	assert.Equal(t, 24*time.Hour, spec.Replay.Lookback)
	assert.Contains(t, spec.Replay.Redeliver, "{{", "the redelivery path keeps its placeholders")
}

func TestReplayWithNothingToAskBackIsRejected(t *testing.T) {
	spec := opsSpec(t, `	<replay interval="5m">
		<list>/deliveries</list>
		<id>id</id>
	</replay>`)
	err := spec.validate()
	require.Error(t, err, "listing failures it cannot ask back is not recovery")
	assert.Contains(t, err.Error(), "redeliver")
}

func TestReplayWithNoIdIsRejected(t *testing.T) {
	spec := opsSpec(t, `	<replay interval="5m">
		<list>/deliveries</list>
		<redeliver>/deliveries/x</redeliver>
	</replay>`)
	err := spec.validate()
	require.Error(t, err, "without an id, every cycle asks for all of them again")
	assert.Contains(t, err.Error(), "<id>")
}

func TestLoadNotifyNeedsEventsToAnnounce(t *testing.T) {
	spec := opsSpec(t, `	<notify path="/_mirror/subs" db="subs.db" retries="3" disable-after="20" timeout="10s"/>`)
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no <events>")
}

func TestLoadNotifyAlongsideEvents(t *testing.T) {
	spec := opsSpec(t, `	<notify path="/_mirror/subs" db="subs.db" retries="2"/>
	<events path="/hook" signature-header="X-Hub-Signature-256" type-header="X-Event">
		<secret>shh</secret>
		<event type="thing.changed" resource="thing" clock="updated_at">
			<subject>thing:<value name="payload.id"/></subject>
			<apply><set field="title">title</set></apply>
		</event>
	</events>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, "subs.db", spec.Notify.DB)
	assert.Equal(t, 2, spec.Notify.Retries)
}

func TestLoadHealth(t *testing.T) {
	spec := opsSpec(t, `	<health live="/.well-known/live" pre-update="/.well-known/pre-update"/>`)
	require.NoError(t, spec.validate())
	assert.Equal(t, "/.well-known/live", spec.Health.Live)
}

func TestHealthNamingNoPathIsRejected(t *testing.T) {
	spec := opsSpec(t, `	<health/>`)
	err := spec.validate()
	require.Error(t, err, "an unregistered path here falls through to the upstream instead of 404ing")
}

func TestUnknownOpsAttributeFailsTheLoad(t *testing.T) {
	for _, body := range []string{
		`	<dashboard path="/_ops" theme="dark"/>`,
		`	<refresh interval="1h" jitter="5m"/>`,
		`	<replay interval="1h" backoff="2x"/>`,
		`	<health live="/live" ready="/ready"/>`,
	} {
		_, err := ParseSpec([]byte(`<mirror name="ops"><upstream base="https://x.example"/>` + body + `</mirror>`))
		assert.Errorf(t, err, "an unread attribute is a typo, and a typo that loads is a setting that silently does nothing: %s", body)
	}
}

func TestReportNamesWhatTheSpecLeavesOut(t *testing.T) {
	spec := opsSpec(t, `	<refresh interval="6h"/>`)
	require.NoError(t, spec.validate())

	out := filepath.Join(t.TempDir(), "report.txt")
	f, err := os.Create(out)
	require.NoError(t, err)
	require.NoError(t, report(spec, f))
	require.NoError(t, f.Close())

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	text := string(raw)
	assert.Contains(t, text, "dashboard at /_mirror/")
	assert.Contains(t, text, "refresh      every 6h0m0s")
	assert.Contains(t, text, "replay       not declared",
		"a mirror with no replay is valid; an author who did not realise that was a choice is not")
	assert.Contains(t, text, "notify       not declared")
}
