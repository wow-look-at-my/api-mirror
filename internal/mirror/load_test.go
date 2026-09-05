package mirror

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wholeSpec exercises every element and attribute the loader understands, in
// file. A parse test per element would pass while the elements refused to
// sit beside each other, which is the only way an author ever writes them.
//
// The XML declaration is omitted the way the shipped files are not: the loader
// strips it, so an inline snippet does not need. Indentation is TABS.
const wholeSpec = `
<mirror name="example">
	<description>Prose for a human. The engine reads none of it.</description>
	<vars>
		<var name="base">https://api.example.com</var>
	</vars>
	<upstream base="{{ .var.base }}">
		<header name="Accept">application/json</header>
		<forward name="Authorization"/>
	</upstream>
	<resource name="repo" ttl="6h">
		<key name="owner" from="owner.login" fold="true"/>
		<key name="name" from="name"/>
		<field name="visibility" type="text">visibility</field>
		<field name="stars" type="int">stargazers_count</field>
		<field name="private" type="bool">private</field>
		<field name="pushed_at" type="time">pushed_at</field>
		<field name="topics" type="json">topics</field>
		<field name="slug" type="text" expr="{{ .owner.login }}/{{ .name }}"/>
		<reveal>
			<public>{{ eq .row.visibility "public" }}</public>
			<probe method="GET" path="/repos/{{ .key.owner }}/{{ .key.name }}"/>
			<grant ttl="24h"/>
			<deny ttl="5m"/>
		</reveal>
	</resource>
	<resource name="readme" ttl="1h" store="document">
		<key name="owner"/>
		<key name="repo"/>
		<drop key="*url"/>
		<keep name="html_url" reason="the docs viewer links a reader back to the rendered page"/>
		<reveal>
			<public>true</public>
		</reveal>
	</resource>
	<route method="get" path="/repos/{owner}/{name}" resource="repo" ttl="30m">
		<accept>application/json</accept>
		<absorb status="404"/>
	</route>
	<route method="GET" path="/orgs/{org}/repos" resource="repo" list="true" complete="true">
		<map param="org" key="owner"/>
		<param name="per_page" type="int" default="30" min="1" max="100"/>
		<param name="type" type="text" key="true"/>
	</route>
	<route method="GET" path="/repos/{owner}/{repo}/readme" resource="readme"/>
	<events path="/webhook" signature-header="X-Hub-Signature-256" type-header="X-Event" reorder-window="2s">
		<secret><value name="env.WEBHOOK_SECRET"/></secret>
		<event type="repository" resource="repo" clock="repository.updated_at">
			<subject>repo:<value name="payload.repository.full_name"/></subject>
			<key field="owner">repository.owner.login</key>
			<key field="name">repository.name</key>
			<apply>
				<set field="visibility">repository.visibility</set>
				<set field="stars" expr="{{ .payload.repository.stargazers_count }}"/>
				<set field="topics" null="allow">repository.topics</set>
			</apply>
		</event>
		<event type="repository_renamed" resource="repo" unordered="true" absorb-when-superseded="true">
			<subject>repo:<value name="payload.repository.full_name"/></subject>
			<key field="owner">repository.owner.login</key>
			<key field="name">changes.repository.name.from</key>
			<invalidate reason="a rename states the old name, never the new full name"/>
		</event>
	</events>
</mirror>
`

func mustParse(t *testing.T, src string) *Spec {
	t.Helper()
	spec, err := ParseSpec([]byte(src))
	require.NoError(t, err, "this source is the loader's own fixture; a parse error here is the loader, not the file")
	return spec
}

// resourceNamed finds a parsed resource, failing rather than returning nil so a
// later assertion cannot panic on a name the loader dropped.
func resourceNamed(t *testing.T, spec *Spec, name string) *Resource {
	t.Helper()
	for _, r := range spec.Resources {
		if r.Name == name {
			return r
		}
	}
	require.FailNowf(t, "resource missing", "the loader parsed no resource named %q", name)
	return nil
}

func TestParseSpec_ReadsTheWholeVocabulary(t *testing.T) {
	spec := mustParse(t, wholeSpec)

	assert.Equal(t, "example", spec.Name, "<mirror name=> names the mirror")
	require.Len(t, spec.Vars, 1, "<description> is prose and must not become a var")
	assert.Equal(t, Var{Name: "base", Value: "https://api.example.com"}, spec.Vars[0])

	assert.Equal(t, "{{ .var.base }}", spec.Upstream.Base,
		"the base stays template source; it is rendered once at start, not at load")
	assert.Equal(t, []Header{{Name: "Accept", Value: "application/json"}}, spec.Upstream.Headers)
	assert.Equal(t, []string{"Authorization"}, spec.Upstream.Forward,
		"a forwarded header is what makes a probe prove the CALLER's access")
	require.Len(t, spec.Resources, 2)
	require.Len(t, spec.Routes, 3)
}

func TestParseSpec_ReadsAColumnsResource(t *testing.T) {
	repo := resourceNamed(t, mustParse(t, wholeSpec), "repo")

	assert.Equal(t, StoreColumns, repo.Store, "a resource with no store= absorbs into columns")
	assert.Equal(t, 6*time.Hour, repo.TTL)
	assert.Equal(t, []Key{
		{Name: "owner", From: "owner.login", Fold: true},
		{Name: "name", From: "name"},
	}, repo.Keys, `fold="true" must survive: without it a differently-cased URL lands on its own row`)

	assert.Equal(t, []Field{
		{Name: "visibility", Type: FieldText, From: "visibility"},
		{Name: "stars", Type: FieldInt, From: "stargazers_count"},
		{Name: "private", Type: FieldBool, From: "private"},
		{Name: "pushed_at", Type: FieldTime, From: "pushed_at"},
		{Name: "topics", Type: FieldJSON, From: "topics"},
		{Name: "slug", Type: FieldText, Expr: "{{ .owner.login }}/{{ .name }}"},
	}, repo.Fields, "declared order is stored order; every write and read builds its statement from it")
}

func TestParseSpec_ReadsADocumentResourceWithItsDropRule(t *testing.T) {
	readme := resourceNamed(t, mustParse(t, wholeSpec), "readme")

	assert.Equal(t, StoreDocument, readme.Store)
	assert.Empty(t, readme.Fields, "a document resource declares no columns")
	assert.Equal(t, []string{"*url"}, readme.Drop,
		"a URL left in a stored document hands the consumer a way around the mirror")
	require.Len(t, readme.Keep, 1)
	assert.Equal(t, "html_url", readme.Keep[0].Name)
	assert.NotEmpty(t, readme.Keep[0].Reason,
		"a keep carries the consumer that needs it, or the drop rule quietly grows a hole")
}

func TestParseSpec_ReadsTheRevealLadder(t *testing.T) {
	rv := resourceNamed(t, mustParse(t, wholeSpec), "repo").Reveal

	require.NotNil(t, rv, "a resource with no reveal cannot be served at all")
	assert.Equal(t, `{{ eq .row.visibility "public" }}`, rv.Public)
	require.NotNil(t, rv.Probe)
	assert.Equal(t, "GET", rv.Probe.Method)
	assert.Equal(t, "/repos/{{ .key.owner }}/{{ .key.name }}", rv.Probe.Path)
	assert.Equal(t, 24*time.Hour, rv.GrantTTL, "a proof that never expires is not a proof")
	assert.Equal(t, 5*time.Minute, rv.DenyTTL)
}

func TestParseSpec_ProbeDefaultsToGET(t *testing.T) {
	spec := mustParse(t, `
<mirror name="m">
	<resource name="r">
		<key name="id"/>
		<field name="a" type="text">a</field>
		<reveal>
			<probe path="/things/{{ .key.id }}"/>
			<grant ttl="1h"/>
			<deny ttl="5m"/>
		</reveal>
	</resource>
</mirror>
`)
	assert.Equal(t, "GET", resourceNamed(t, spec, "r").Reveal.Probe.Method,
		"a probe only ever reads, so the method a spec leaves out is GET")
}

func TestParseSpec_ReadsRoutes(t *testing.T) {
	spec := mustParse(t, wholeSpec)

	one := spec.Routes[0]
	assert.Equal(t, "GET", one.Method, `method="get" is upcased, so a route matches the request line`)
	assert.Equal(t, "/repos/{owner}/{name}", one.Path)
	assert.Equal(t, "repo", one.Resource)
	assert.Equal(t, 30*time.Minute, one.TTL, "a route's own ttl overrides its resource's")
	assert.False(t, one.List)
	assert.Equal(t, []string{"application/json"}, one.Accept)
	assert.Equal(t, []int{404}, one.Absorb,
		"a named 4xx is the upstream stating a fact, and the spec says which")

	list := spec.Routes[1]
	assert.True(t, list.List, `list="true" makes the answer an array of rows`)
	assert.True(t, list.Complete,
		`complete="true" says the answer is the whole set, which is what licenses a delete`)
	assert.Equal(t, map[string]string{"org": "owner"}, list.Params,
		"<map> renames a path parameter onto the resource key it supplies")
	assert.Equal(t, []QueryParam{
		{Name: "per_page", Type: FieldInt, Default: "30", Min: 1, Max: 100},
		{Name: "type", Type: FieldText, Key: true},
	}, list.Query)
}

func TestParseSpec_RouteMethodDefaultsToGET(t *testing.T) {
	spec := mustParse(t, `<mirror name="m"><route path="/things"/></mirror>`)
	assert.Equal(t, "GET", spec.Routes[0].Method, "a route with no method reads")
}

func TestParseSpec_ReadsEvents(t *testing.T) {
	ev := mustParse(t, wholeSpec).Events

	require.NotNil(t, ev)
	assert.Equal(t, "/webhook", ev.Path)
	assert.Equal(t, "{{ .env.WEBHOOK_SECRET }}", ev.Secret,
		"the secret stays template source so it is resolved from the environment at start")
	assert.Equal(t, "X-Hub-Signature-256", ev.SignatureHeader)
	assert.Equal(t, "X-Event", ev.TypeHeader)
	assert.Equal(t, 2*time.Second, ev.ReorderWindow)
	require.Len(t, ev.List, 2)

	applied := ev.List[0]
	assert.Equal(t, "repository", applied.Type)
	assert.Equal(t, "repo", applied.Resource)
	assert.Equal(t, "repo:{{ .payload.repository.full_name }}", applied.Subject,
		"a subject mixes literal text with placeholders, and both halves must survive")
	assert.Equal(t, "repository.updated_at", applied.Clock)
	assert.False(t, applied.Unordered)
	assert.Equal(t, []Set{
		{Field: "visibility", From: "repository.visibility"},
		{Field: "stars", Expr: "{{ .payload.repository.stargazers_count }}"},
		{Field: "topics", From: "repository.topics", AllowNull: true},
	}, applied.Sets, `null="allow" is the opt-in that lets a stated null be written`)
	assert.Nil(t, applied.Invalidate)

	dropped := ev.List[1]
	assert.True(t, dropped.Unordered, "an event with no clock must say so out loud")
	assert.True(t, dropped.AbsorbWhenSuperseded)
	require.NotNil(t, dropped.Invalidate)
	assert.NotEmpty(t, dropped.Invalidate.Reason,
		"an invalidation states why the payload could not answer")
}

func TestParseSpec_ZeroReorderWindowIsARealChoice(t *testing.T) {
	for _, written := range []string{"0", "0s"} {
		spec := mustParse(t, `
<mirror name="m">
	<events path="/w" signature-header="S" type-header="T" reorder-window="`+written+`">
		<secret>x</secret>
	</events>
</mirror>
`)
		assert.Zero(t, spec.Events.ReorderWindow,
			"%q means dispatch on arrival and leave ordering to the watermark, not a parse error", written)
	}
}

// The whole spec must also MEAN something the engine can serve. Parsing it and
// stopping there would let the loader and validate drift apart.
func TestParseSpec_WholeVocabularyAlsoValidates(t *testing.T) {
	require.NoError(t, mustParse(t, wholeSpec).validate())
}

// The shipped specs are the project's own documentation. If either stops
// loading, the file a reader is told to copy no longer works.
func TestLoad_ShippedSpecs(t *testing.T) {
	for _, path := range shippedSpecs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			spec, err := Load(path)
			require.NoErrorf(t, err, "%s ships as a worked example; it must parse and validate", path)
			assert.NotEmpty(t, spec.Name)
			assert.NotEmpty(t, spec.Resources, "a mirror that stores nothing mirrors nothing")
			assert.NotEmpty(t, spec.Routes, "a mirror with no route answers nothing from cache")
			assert.NotEmpty(t, spec.Fingerprint(),
				"the fingerprint is derived from the DDL, so a loadable spec always has one")
		})
	}
}

func TestLoad_MissingFileNamesThePath(t *testing.T) {
	_, err := Load("no-such-spec.xml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-spec.xml",
		"an author who mistyped a path needs the path back")
}

func TestLoad_ParseErrorAndValidationErrorAreToldApart(t *testing.T) {
	dir := t.TempDir()
	bad := dir + "/bad.xml"
	require.NoError(t, os.WriteFile(bad, []byte(`<mirror name="m"><nope/></mirror>`), 0o600))
	_, err := Load(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse spec", "a shape error is reported as a parse failure")

	meaningless := dir + "/meaningless.xml"
	require.NoError(t, os.WriteFile(meaningless, []byte(`
<mirror name="m">
	<upstream base="https://api.example.com"/>
	<resource name="r">
		<key name="id"/>
		<field name="a" type="text">a</field>
	</resource>
</mirror>
`), 0o600))
	_, err = Load(meaningless)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate spec",
		"a well-formed file that means nothing servable is reported as a validation failure")
	assert.Contains(t, err.Error(), "who may read it",
		"the message names the design mistake, not the syntax")
}

// Every rejection below is a typo an author makes. The loader's job is to name
// it at load, where the author is standing, instead of ignoring it silently and
// serving a mirror that does not do what the file says.
func TestParseSpec_Rejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a root element that is not <mirror>",
		src:  `<mirrors name="m"/>`,
		want: "root element is <mirrors>",
	}, {
		name: "an unknown child of <mirror>",
		src:  `<mirror name="m"><resources/></mirror>`,
		want: "unexpected child element <resources>",
	}, {
		name: "an unknown attribute on <mirror>",
		src:  `<mirror name="m" version="2"/>`,
		want: `unknown attribute "version"`,
	}, {
		name: "an unknown attribute on <resource>",
		src:  `<mirror name="m"><resource name="r" cache="yes"/></mirror>`,
		want: `unknown attribute "cache"`,
	}, {
		name: "a <var> with no name",
		src:  `<mirror name="m"><vars><var>x</var></vars></mirror>`,
		want: "name is required",
	}, {
		name: "an unknown child of <vars>",
		src:  `<mirror name="m"><vars><const name="a">x</const></vars></mirror>`,
		want: "unexpected child element <const>",
	}, {
		name: "a <forward> with no name",
		src:  `<mirror name="m"><upstream base="u"><forward/></upstream></mirror>`,
		want: "name is required",
	}, {
		name: "a <header> with no name",
		src:  `<mirror name="m"><upstream base="u"><header>v</header></upstream></mirror>`,
		want: "name is required",
	}, {
		name: "an unknown child of <upstream>",
		src:  `<mirror name="m"><upstream base="u"><retry/></upstream></mirror>`,
		want: "unexpected child element <retry>",
	}, {
		name: "a <drop> naming its pattern as text",
		src:  `<mirror name="m"><resource name="r"><drop>url</drop></resource></mirror>`,
		want: `write <drop key="url"/>`,
	}, {
		name: "a <drop> naming no pattern at all",
		src:  `<mirror name="m"><resource name="r"><drop/></resource></mirror>`,
		want: "<drop> needs a key= pattern",
	}, {
		name: "a resource ttl that is not a duration",
		src:  `<mirror name="m"><resource name="r" ttl="soon"/></mirror>`,
		want: `"soon" is not a duration`,
	}, {
		name: "a route ttl that is not a duration",
		src:  `<mirror name="m"><route path="/x" ttl="never"/></mirror>`,
		want: "is not a duration",
	}, {
		name: "an unknown child of <resource>",
		src:  `<mirror name="m"><resource name="r"><column name="a"/></resource></mirror>`,
		want: "unexpected child element <column>",
	}, {
		name: "an unknown child of <reveal>",
		src:  `<mirror name="m"><resource name="r"><reveal><allow/></reveal></resource></mirror>`,
		want: "unexpected child element <allow>",
	}, {
		name: "a <grant> with no ttl",
		src:  `<mirror name="m"><resource name="r"><reveal><grant/></reveal></resource></mirror>`,
		want: "<grant>: ttl is required",
	}, {
		name: "a <deny> ttl that is not a duration",
		src:  `<mirror name="m"><resource name="r"><reveal><deny ttl="a while"/></reveal></resource></mirror>`,
		want: "is not a duration",
	}, {
		name: "an unknown child of <route>",
		src:  `<mirror name="m"><route path="/x"><header name="a"/></route></mirror>`,
		want: "unexpected child element <header>",
	}, {
		name: "an absorb status that is not a number",
		src:  `<mirror name="m"><route path="/x"><absorb status="gone"/></route></mirror>`,
		want: `<absorb status="gone"> is not a number`,
	}, {
		name: "a query bound that is not a number",
		src:  `<mirror name="m"><route path="/x"><param name="page" type="int" min="one"/></route></mirror>`,
		want: `min="one" is not a number`,
	}, {
		name: "a reorder window past the cap",
		src: `<mirror name="m"><events path="/w" reorder-window="10s">` +
			`<secret>x</secret></events></mirror>`,
		want: "longer than the 5s cap",
	}, {
		name: "a reorder window that is not a duration",
		src:  `<mirror name="m"><events path="/w" reorder-window="soon"><secret>x</secret></events></mirror>`,
		want: "is not a duration",
	}, {
		name: "an unknown attribute on <route>",
		src:  `<mirror name="m"><route path="/x" cache="60"/></mirror>`,
		want: `unknown attribute "cache"`,
	}, {
		name: "an unknown attribute on <events>",
		src:  `<mirror name="m"><events path="/w" secret="hunter2"><secret>x</secret></events></mirror>`,
		want: `unknown attribute "secret"`,
	}, {
		name: "an unknown child of <events>",
		src:  `<mirror name="m"><events path="/w"><token>x</token></events></mirror>`,
		want: "unexpected child element <token>",
	}, {
		name: "an unknown child of <event>",
		src:  `<mirror name="m"><events path="/w"><event type="push"><when/></event></events></mirror>`,
		want: "unexpected child element <when>",
	}, {
		name: "an unknown attribute on <event>",
		src:  `<mirror name="m"><events path="/w"><event type="push" retry="3"/></events></mirror>`,
		want: `unknown attribute "retry"`,
	}, {
		name: "an <apply> holding anything but <set>",
		src: `<mirror name="m"><events path="/w"><event type="push"><apply><clear field="a"/>` +
			`</apply></event></events></mirror>`,
		want: "<apply> holds <set> elements, not <clear>",
	}, {
		name: `a null= that is not "allow"`,
		src: `<mirror name="m"><events path="/w"><event type="push"><apply>` +
			`<set field="a" null="true">x</set></apply></event></events></mirror>`,
		want: `null="true" -- the only value is "allow"`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSpec([]byte(tc.src))
			require.Error(t, err, "the loader accepted %s, so the mistake reaches run time instead", tc.name)
			assert.Contains(t, err.Error(), tc.want,
				"the message must name what is wrong; an author reads it instead of the loader")
		})
	}
}
