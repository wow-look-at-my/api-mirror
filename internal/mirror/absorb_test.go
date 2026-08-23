package mirror

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonValue decodes a fragment the way a fetch decodes an upstream body, with
// numbers left as json.Number. A test that hands coerce a plain Go float tests a
// value the engine never actually receives.
func jsonValue(t *testing.T, src string) any {
	t.Helper()
	v, err := decodeJSON([]byte(src))
	require.NoError(t, err)
	return v
}

// A stored cell must come back as the JSON shape the consumer was promised. The
// round trip is the assertion: a column that absorbs cleanly and presents wrong
// serves a document whose shape depends on cache state.
func TestCoerceAndPresent_RoundTripEveryType(t *testing.T) {
	cases := []struct {
		name   string
		typ    FieldType
		in     string // JSON, as an upstream sends it
		stored any    // what the column holds
		out    any    // what a consumer receives back
	}{
		{name: "text", typ: FieldText, in: `"hello"`, stored: "hello", out: "hello"},
		{name: "int", typ: FieldInt, in: `42`, stored: int64(42), out: int64(42)},
		{name: "bool true", typ: FieldBool, in: `true`, stored: int64(1), out: true},
		{name: "bool false", typ: FieldBool, in: `false`, stored: int64(0), out: false},
		{
			name: "time as RFC 3339", typ: FieldTime,
			in: `"2026-01-02T03:04:05Z"`, stored: int64(1767323045), out: "2026-01-02T03:04:05Z",
		},
		{
			name: "time as a numeric epoch", typ: FieldTime,
			in: `1767323045`, stored: int64(1767323045), out: "2026-01-02T03:04:05Z",
		},
		{
			name: "json object", typ: FieldJSON,
			in:     `{"a":1}`,
			stored: `{"a":1}`,
			out:    map[string]any{"a": json.Number("1")},
		},
		{
			name: "json array", typ: FieldJSON,
			in:     `["go","xml"]`,
			stored: `["go","xml"]`,
			out:    []any{"go", "xml"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerce(tc.typ, jsonValue(t, tc.in))
			require.NoError(t, err)
			assert.Equal(t, tc.stored, got, "a %s column stores this shape, and every read builds on it", tc.typ)

			assert.Equal(t, tc.out, present(Field{Name: "f", Type: tc.typ}, got),
				"a consumer must get its declared JSON shape back, not the storage encoding")
		})
	}
}

// Both encodings of a time land on the same second. An upstream uses both,
// sometimes for the same field, so a reader that handles one silently loses
// every value in the other shape.
func TestCoerceTime_StringAndEpochAgree(t *testing.T) {
	fromString, err := coerce(FieldTime, jsonValue(t, `"2026-01-02T03:04:05Z"`))
	require.NoError(t, err)
	fromEpoch, err := coerce(FieldTime, jsonValue(t, `1767323045`))
	require.NoError(t, err)
	assert.Equal(t, fromString, fromEpoch,
		"the two encodings are the same moment; storing them differently splits one fact in two")
}

func TestCoerce_NilStaysNilSoTheCallerDecides(t *testing.T) {
	for _, typ := range []FieldType{FieldText, FieldInt, FieldBool, FieldTime, FieldJSON} {
		got, err := coerce(typ, nil)
		require.NoError(t, err)
		assert.Nil(t, got, "absent is a value; only the caller knows whether to write it over a %s column", typ)
	}
}

// A value the column cannot hold is an error, never a zero. A zero written here
// is indistinguishable from a real zero upstream, which is the quietest way a
// mirror serves a wrong number.
func TestCoerce_RefusesWhatTheTypeCannotHold(t *testing.T) {
	cases := []struct {
		name string
		typ  FieldType
		in   string
		want string
	}{
		{name: "an object in an int", typ: FieldInt, in: `{"a":1}`, want: "not an integer"},
		{name: "a word in an int", typ: FieldInt, in: `"twelve"`, want: "not an integer"},
		{name: "a fraction in an int", typ: FieldInt, in: `1.5`, want: "not an integer"},
		{name: "an object in a bool", typ: FieldBool, in: `{"a":1}`, want: "not a bool"},
		{name: "a word in a time", typ: FieldTime, in: `"yesterday"`, want: "not a time"},
		{name: "an object in a time", typ: FieldTime, in: `{"a":1}`, want: "not a time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := coerce(tc.typ, jsonValue(t, tc.in))
			require.Error(t, err, "coercing %s produced a value; a wrong number is worse than a refused write", tc.name)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	_, err := coerce(FieldType("colour"), "red")
	require.Error(t, err, "a type validate somehow let through must still fail loudly here")
	assert.Contains(t, err.Error(), "unknown field type")
}

func TestCoerce_AcceptsTheLooseShapesAnUpstreamSends(t *testing.T) {
	got, err := coerce(FieldText, jsonValue(t, `7`))
	require.NoError(t, err)
	assert.Equal(t, "7", got, "a number in a text column keeps its digits, not a float's rendering")

	got, err = coerce(FieldInt, jsonValue(t, `"12"`))
	require.NoError(t, err)
	assert.Equal(t, int64(12), got, "an upstream that quotes its ids still means the number")

	got, err = coerce(FieldBool, jsonValue(t, `"true"`))
	require.NoError(t, err)
	assert.Equal(t, int64(1), got, "a quoted boolean is the same answer")

	got, err = coerce(FieldTime, jsonValue(t, `""`))
	require.NoError(t, err)
	assert.Nil(t, got, "an empty time string states nothing, so it stores nothing")
}

// present is asked to render whatever the database handed back, which is not
// always what absorb wrote. It must degrade to the raw cell rather than panic.
func TestPresent_PassesThroughACellItCannotDecode(t *testing.T) {
	assert.Nil(t, present(Field{Type: FieldBool}, nil), "a null column presents as null")
	assert.Equal(t, "yes", present(Field{Type: FieldBool}, "yes"),
		"a bool column holding text is served as-is rather than crashing the read")
	assert.Equal(t, "not json", present(Field{Type: FieldJSON}, "not json"),
		"a json column holding an unparseable string is served as the string")
	assert.Equal(t, int64(3), present(Field{Type: FieldJSON}, int64(3)),
		"a json column holding a number never went through the encoder, so nothing decodes it")
	assert.Equal(t, "plain", present(Field{Type: FieldText}, "plain"))
}

func TestAbsorbAndRebuild_ProjectOnlyTheDeclaredFields(t *testing.T) {
	res := &Resource{
		Name:  "repo",
		Store: StoreColumns,
		Keys:  []Key{{Name: "owner", From: "owner.login", Fold: true}},
		Fields: []Field{
			{Name: "stars", Type: FieldInt, From: "stargazers_count"},
			{Name: "live", Type: FieldBool, From: "archived"},
			{Name: "slug", Type: FieldText, Expr: "{{ .owner.login }}/{{ .name }}"},
		},
	}
	doc := jsonValue(t, `{
		"owner": {"login": "WowLookAtMy"},
		"name": "api-mirror",
		"stargazers_count": 5,
		"archived": false,
		"secret_field": "never declared"
	}`)

	row, err := absorb(res, doc)
	require.NoError(t, err)
	assert.Equal(t, "wowlookatmy", row["owner"], `fold="true" lower-cases the key before it is stored`)
	assert.Equal(t, int64(5), row["stars"])
	assert.Equal(t, int64(0), row["live"])
	assert.Equal(t, "WowLookAtMy/api-mirror", row["slug"], "an expr field renders against the whole document")
	assert.NotContains(t, row, "secret_field",
		"a field the spec does not declare is a field the mirror never stores and never serves")

	out, err := rebuild(res, row)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"owner": "wowlookatmy",
		"stars": int64(5),
		"live":  false,
		"slug":  "WowLookAtMy/api-mirror",
	}, out, "the rebuilt answer's shape is the spec's, not whatever the upstream added this week")
}

func TestAbsorb_ReportsWhichFieldRefusedTheValue(t *testing.T) {
	res := &Resource{
		Name:   "repo",
		Store:  StoreColumns,
		Keys:   []Key{{Name: "id", From: "id"}},
		Fields: []Field{{Name: "stars", Type: FieldInt, From: "stars"}},
	}
	_, err := absorb(res, jsonValue(t, `{"id":"1","stars":"lots"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo.stars",
		"the message names the resource and the column, which is what an author has to go and fix")
}

func TestRebuild_RefusesADocumentResource(t *testing.T) {
	_, err := rebuild(&Resource{Name: "readme", Store: StoreDocument}, Row{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replays its stored document",
		"a document resource has no columns to rebuild from, and guessing one would invent an answer")
}

// A URL in a stored document is a way around the mirror: a consumer follows it,
// and that request is one the mirror did not cache, did not gate and cannot see.
func TestTrim_DropsBySuffixAndByExactName(t *testing.T) {
	res := &Resource{
		Drop: []string{"*url", "node_id"},
		Keep: []Keep{{Name: "html_url", Reason: "the viewer links a reader back to the page"}},
	}
	doc := jsonValue(t, `{
		"name": "api-mirror",
		"url": "https://upstream/repos/1",
		"html_url": "https://upstream/wow/api-mirror",
		"AVATAR_URL": "https://upstream/a.png",
		"node_id": "MDEw",
		"owner": {"login": "wow", "url": "https://upstream/users/wow"},
		"tags": [{"name": "v1", "commit_url": "https://upstream/c/1"}]
	}`)

	got, ok := trim(res, doc).(map[string]any)
	require.True(t, ok)

	assert.NotContains(t, got, "url", "an exact match on a drop pattern goes")
	assert.NotContains(t, got, "AVATAR_URL", "a drop pattern matches without regard to case")
	assert.NotContains(t, got, "node_id", "a plain name is an exact pattern")
	assert.Equal(t, "https://upstream/wow/api-mirror", got["html_url"],
		"a <keep> rescues one named key from the pattern that would otherwise take it")
	assert.Equal(t, "api-mirror", got["name"], "a key no pattern names is untouched")

	nested, ok := got["owner"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, nested, "url", "the trim recurses; a URL one level down is still a way out")
	assert.Equal(t, "wow", nested["login"])

	tags, ok := got["tags"].([]any)
	require.True(t, ok)
	require.Len(t, tags, 1)
	item, ok := tags[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, item, "commit_url", "the trim walks into arrays too")
	assert.Equal(t, "v1", item["name"])
}

func TestTrim_WithNoDropRuleReturnsTheDocumentUntouched(t *testing.T) {
	doc := jsonValue(t, `{"url":"https://upstream/1"}`)
	assert.Equal(t, doc, trim(&Resource{}, doc),
		"a resource that declares no drop keeps everything; the rule is opt-in and says so")
}

func TestDropped_MatchesTheTwoDeclaredPatternForms(t *testing.T) {
	drop := []string{"*_url", "id"}
	assert.True(t, dropped("avatar_url", drop))
	assert.True(t, dropped("ID", drop), "a pattern is matched case-insensitively")
	assert.False(t, dropped("url", drop), `"*_url" needs the underscore; a suffix pattern is not a substring`)
	assert.False(t, dropped("identifier", drop), "an exact pattern does not match a longer name")
}

// One marshaller writes every document the mirror stores, so bytes written by a
// fetch, rewritten by a delivery, and served on a hit are the same bytes.
func TestMarshalJSON_DoesNotEscapeHTMLAndDropsTheTrailingNewline(t *testing.T) {
	b, err := marshalJSON(map[string]any{"name": "tom & jerry <b>"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"tom & jerry <b>"}`, string(b))
	assert.Equal(t, `{"name":"tom & jerry <b>"}`, string(b),
		"an upstream does not escape, so a name containing & must survive the round trip byte for byte")
}

func TestDecodeJSON_KeepsALargeIDExact(t *testing.T) {
	v, err := decodeJSON([]byte(`{"id":9007199254740993}`))
	require.NoError(t, err)
	doc, ok := v.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, json.Number("9007199254740993"), doc["id"],
		"decoding into a float would round this id, and the mirror would store a different fact")

	_, err = decodeJSON([]byte(`{`))
	require.Error(t, err, "a truncated body must fail rather than absorb as an empty document")
}
