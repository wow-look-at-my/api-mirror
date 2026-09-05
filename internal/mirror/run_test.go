package mirror

import (
	"context"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot reaches the shipped specs from this package. A test runs in its own
const repoRoot = "../.."

// shippedSpecs are the worked examples, as paths a test can open.
var shippedSpecs = []string{
	filepath.Join(repoRoot, "mirror.example.xml"),
	filepath.Join(repoRoot, "samples", "github", "github.xml"),
}

// withArgs gives Run its own command line and puts the process's back. Run
// parses the global flag set, so tests in binary would otherwise
// redefine the same flags and panic.
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	savedArgs, savedFlags := os.Args, flag.CommandLine
	t.Cleanup(func() {
		os.Args = savedArgs
		flag.CommandLine = savedFlags
	})
	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = args
}

// captureStdout redirects os.Stdout to a file for the length of a test and
// returns what was written. report writes to an *os.File, so a bytes.Buffer
// cannot stand in for it.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*")
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = f
	t.Cleanup(func() {
		os.Stdout = saved
		f.Close()
	})
	return func() string {
		require.NoError(t, f.Sync())
		b, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		return string(b)
	}
}

// writeSpec drops a spec source in this test's own directory.
func writeSpec(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mirror.xml")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
	return path
}

// runnableSpec is a whole mirror, events included, so a start-up test exercises
// the ingest wiring rather than only the serving half.
const runnableSpec = `
<mirror name="runnable">
	<upstream base="https://api.example.com"/>
	<resource name="thing" ttl="1h">
		<key name="id"/>
		<field name="title" type="text">title</field>
		<reveal>
			<public>true</public>
		</reveal>
	</resource>
	<route method="GET" path="/things/{id}" resource="thing"/>
	<events path="/webhook" signature-header="X-Signature" type-header="X-Event">
		<secret>a-test-secret</secret>
		<event type="thing" resource="thing" clock="thing.updated_at">
			<subject>thing:<value name="payload.thing.id"/></subject>
			<key field="id">thing.id</key>
			<apply>
				<set field="title">thing.title</set>
			</apply>
		</event>
	</events>
</mirror>
`

// report is what an author reads before running anything, so it has to name
// every derived thing: the fingerprint that decides a nuke, the tables, the
// routes, and the deliveries the mirror will accept.
func TestReport_NamesEverythingTheSpecDerives(t *testing.T) {
	spec, err := ParseSpec([]byte(runnableSpec))
	require.NoError(t, err)
	require.NoError(t, spec.validate())

	out, err := os.CreateTemp(t.TempDir(), "report-*")
	require.NoError(t, err)
	require.NoError(t, report(spec, out))
	require.NoError(t, out.Sync())
	raw, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	text := string(raw)

	assert.Contains(t, text, "mirror runnable", "the report opens with which mirror it is about")
	assert.Contains(t, text, "schema fingerprint: "+spec.Fingerprint(),
		"the fingerprint decides whether the next start nukes the cache, so an author must be able to see it")
	assert.Contains(t, text, "CREATE TABLE res_thing",
		"the derived table is the whole point of declaring a resource")
	assert.Contains(t, text, "mirror_freshness", "the engine's own tables are part of the derived schema")
	assert.Contains(t, text, "routes:")
	assert.Contains(t, text, "/things/{id}")
	assert.Contains(t, text, "-> thing (one)",
		"the report says whether a route answers one row or a list, which changes the answer's shape")
	assert.Contains(t, text, "events at /webhook",
		"a reader has to know where the upstream is expected to post before they configure it")
}

func TestReport_MarksAListRouteAsAList(t *testing.T) {
	spec, err := ParseSpec([]byte(`
<mirror name="m">
	<upstream base="https://api.example.com"/>
	<resource name="thing">
		<key name="id"/>
		<field name="title" type="text">title</field>
		<reveal><public>true</public></reveal>
	</resource>
	<route method="GET" path="/things" resource="thing" list="true"/>
</mirror>
`))
	require.NoError(t, err)

	out, err := os.CreateTemp(t.TempDir(), "report-*")
	require.NoError(t, err)
	require.NoError(t, report(spec, out))
	raw, err := os.ReadFile(out.Name())
	require.NoError(t, err)

	assert.Contains(t, string(raw), "(list)",
		"a list route answers an array; a report that hid that would describe a different API")
}

func TestReport_OmitsTheEventSectionWhenNothingIsDeclared(t *testing.T) {
	spec, err := ParseSpec([]byte(`
<mirror name="m">
	<upstream base="https://api.example.com"/>
	<route method="GET" path="/things" resource="thing"/>
</mirror>
`))
	require.NoError(t, err)

	out, err := os.CreateTemp(t.TempDir(), "report-*")
	require.NoError(t, err)
	require.NoError(t, report(spec, out))
	raw, err := os.ReadFile(out.Name())
	require.NoError(t, err)

	assert.NotContains(t, string(raw), "events at",
		"a mirror with no ingest must not print an empty events heading a reader would go looking for")
}

// --check is the whole reason the flag exists: load, say what it derives, exit
// clean, and never open a socket or a database.
func TestRun_CheckLoadsTheSpecAndExitsClean(t *testing.T) {
	path := writeSpec(t, runnableSpec)
	withArgs(t, "api-mirror", "--spec", path, "--check")
	read := captureStdout(t)

	require.NoError(t, Run(), "--check on a valid spec is a clean exit")
	assert.Contains(t, read(), "mirror runnable", "--check prints the report rather than serving")
}

func TestRun_CheckAlsoWorksOnTheShippedExample(t *testing.T) {
	withArgs(t, "api-mirror", "--spec", shippedSpecs[0], "--check")
	read := captureStdout(t)

	require.NoError(t, Run(), "the shipped example is what a reader is told to run first")
	out := read()
	assert.Contains(t, out, "mirror placeholder")
	assert.Contains(t, out, "CREATE TABLE res_user")
}

func TestRun_ReportsASpecItCannotLoad(t *testing.T) {
	withArgs(t, "api-mirror", "--spec", filepath.Join(t.TempDir(), "absent.xml"), "--check")
	err := Run()
	require.Error(t, err, "a missing spec must stop the process, not start a mirror of nothing")
	assert.Contains(t, err.Error(), "absent.xml")

	bad := writeSpec(t, `<mirror name="m"><upstream base="u"/><resource name="r"><key name="id"/></resource></mirror>`)
	withArgs(t, "api-mirror", "--spec", bad, "--check")
	err = Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate spec",
		"a spec that parses but means nothing servable is refused at start, where the author can see it")
}

// A listen address already in use is the start-up failure a test can force
// without a signal. It drives everything before the socket -- the store, the
// engine, the ingest -- and then proves the failure is returned rather than
// logged and forgotten.
func TestRun_ReturnsAListenFailureAfterWiringEverything(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { held.Close() })

	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.xml")
	require.NoError(t, os.WriteFile(path, []byte(runnableSpec), 0o600))

	withArgs(t, "api-mirror",
		"--spec", path,
		"--db", filepath.Join(dir, "cache.db"),
		"--listen", held.Addr().String())

	err = Run()
	require.Error(t, err, "a mirror that cannot listen must exit non-zero, not sit there serving nobody")
	assert.Contains(t, strings.ToLower(err.Error()), "address already in use")

	assert.FileExists(t, filepath.Join(dir, "cache.db"),
		"the store is opened before the socket, so the database exists even on a failed start")
}

func TestRun_ReportsAStoreItCannotOpen(t *testing.T) {
	// The listen address is already in use, so this test cannot hang
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { held.Close() })

	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.xml")
	require.NoError(t, os.WriteFile(path, []byte(runnableSpec), 0o600))

	withArgs(t, "api-mirror",
		"--spec", path,
		// The spec file is not a directory, so nothing can be created inside it.
		"--db", filepath.Join(path, "cache.db"),
		"--listen", held.Addr().String())

	err = Run()
	require.Error(t, err, "a database path that cannot be created must stop the start, not serve an empty cache")
	assert.NotContains(t, strings.ToLower(err.Error()), "address already in use",
		"the store is opened before the socket, so this is the store's failure that surfaced")
}

// A shipped spec is a runnable file, not only a loadable. Building an engine
// and an ingest from it is what proves the vars, the upstream and the webhook
// secret a reader copies actually resolve at start.
func TestShippedSpecsBuildAnEngine(t *testing.T) {
	for _, path := range shippedSpecs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			spec, err := Load(path)
			require.NoError(t, err)

			store, err := Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"), spec)
			require.NoError(t, err)
			t.Cleanup(func() { store.Close() })

			engine, err := NewEngine(spec, store, nil)
			require.NoErrorf(t, err, "%s must produce a working engine, or it cannot be run as documented", path)
			assert.NotEmpty(t, engine.up.base,
				"the base comes from a var, so this is also the proof that a spec's vars reach the templates reading them")

			if spec.Events != nil {
				_, err := NewIngest(spec, store, engine.vars)
				require.NoError(t, err, "the ingest half of the file has to start too")
			}
		})
	}
}

func TestEngineDrain_IsSafeWithNothingInFlight(t *testing.T) {
	spec, err := ParseSpec([]byte(runnableSpec))
	require.NoError(t, err)
	require.NoError(t, spec.validate())

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"), spec)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	engine, err := NewEngine(spec, store, nil)
	require.NoError(t, err)
	assert.True(t, engine.Drain(drainTimeout), "an idle engine drains at once")

	ingest, err := NewIngest(spec, store, engine.vars)
	require.NoError(t, err)
	engine.SetIngest(ingest)
	assert.True(t, engine.Drain(drainTimeout),
		"a shutdown waits for deliveries too; an upstream never sends one twice")
}
