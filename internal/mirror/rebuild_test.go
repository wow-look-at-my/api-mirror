package mirror

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebuild_NestsEachFieldAtItsSourcePath(t *testing.T) {
	res := &Resource{
		Name:  "branch",
		Store: StoreColumns,
		Keys:  []Key{{Name: "ref"}},
		Fields: []Field{
			{Name: "full_ref", Type: FieldText, From: "ref"},
			{Name: "tip_sha", Type: FieldText, From: "object.sha"},
			{Name: "tip_type", Type: FieldText, From: " object . type "},
		},
	}
	out, err := rebuild(res, Row{"ref": "main", "full_ref": "refs/heads/main", "tip_sha": "abc", "tip_type": "commit"})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"ref":    "refs/heads/main",
		"object": map[string]any{"sha": "abc", "type": "commit"},
	}, out, "the key's own value is the route's spelling, never the upstream's, so only the field answers")
}

func TestRebuild_ServesADocumentKeyAtItsPathAndType(t *testing.T) {
	res := &Resource{
		Name:   "pull",
		Store:  StoreColumns,
		Keys:   []Key{{Name: "owner", From: "base.repo.owner.login", Fold: true}, {Name: "repo"}, {Name: "number", From: "number", Type: FieldInt}},
		Fields: []Field{{Name: "title", Type: FieldText, From: "title"}},
	}
	row, err := absorb(res, jsonValue(t, `{"number":7,"title":"t","base":{"repo":{"owner":{"login":"Octo"}}}}`))
	require.NoError(t, err)
	assert.Equal(t, "octo", row["owner"], "the key folds, so a differently cased URL finds the row")
	row["repo"] = "demo"

	out, err := rebuild(res, row)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"number": int64(7),
		"title":  "t",
		"base":   map[string]any{"repo": map[string]any{"owner": map[string]any{"login": "Octo"}}},
	}, out, "a key read from the document is served as sent and typed; the route-only repo key is not served")

	delete(row, Key{Name: "owner", From: "x", Fold: true}.sentColumn())
	out, err = rebuild(res, row)
	require.NoError(t, err)
	assert.NotContains(t, out, "base", "with no as-sent spelling stored, the folded value is not the upstream's word")
}

func TestValidate_RefusesOneFactReadTwice(t *testing.T) {
	res := &Resource{
		Name: "pull", Store: StoreColumns, Reveal: &Reveal{Public: "true"},
		Keys:   []Key{{Name: "number", From: "number", Type: FieldInt}},
		Fields: []Field{{Name: "pr_number", Type: FieldInt, From: "number"}},
	}
	err := res.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declare one fact once")

	ev := &Event{Type: "pull_request", Sets: []Set{
		{Field: "number", From: "number"},
		{Field: "pr_number", From: " number "},
	}}
	err = validateEventSources(ev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store one fact once")

	assert.NoError(t, validateEventSources(&Event{Type: "push",
		Keys: []Set{{Field: "owner", From: "repository.owner.login"}},
		Sets: []Set{{Field: "owner", From: "repository.owner.login"}, {Field: "ref", Expr: "{{ .payload.ref }}"}},
	}), "a key and a set naming the same column are one declaration of one fact")
}

func TestValidate_RefusesAKeyTypeThatIsNotTextOrInt(t *testing.T) {
	res := &Resource{
		Name: "r", Store: StoreColumns, Reveal: &Reveal{Public: "true"},
		Keys:   []Key{{Name: "id", From: "id", Type: FieldBool}},
		Fields: []Field{{Name: "a", Type: FieldText, From: "a"}},
	}
	err := res.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a key is text or int")
}

func TestValidate_RefusesFieldsThatRebuildAtOnePath(t *testing.T) {
	base := func(fields ...Field) *Resource {
		return &Resource{
			Name: "r", Store: StoreColumns, Keys: []Key{{Name: "id"}}, Fields: fields,
			Reveal: &Reveal{Public: "true"},
		}
	}
	for name, res := range map[string]*Resource{
		"the same path":       base(Field{Name: "a", Type: FieldText, From: "x"}, Field{Name: "b", Type: FieldInt, From: "x"}),
		"one under the other": base(Field{Name: "a", Type: FieldJSON, From: "owner"}, Field{Name: "b", Type: FieldText, From: "owner.login"}),
	} {
		err := res.validate()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "declare one fact once", name)
	}

	err := base(Field{Name: "a", Type: FieldText, Expr: "{{ .x }}"}).validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "its answer would be empty")

	assert.NoError(t, base(Field{Name: "a", Type: FieldText, From: "owner.login"}, Field{Name: "b", Type: FieldText, From: "owner_login"}).validate(),
		"a shared prefix of characters is not a shared path")
}

// sampleOf is a value an upstream could send for a column of type t. Text is
// mixed case, so a folded value cannot pass for the original.
func sampleOf(t FieldType, path string) any {
	switch t {
	case FieldInt:
		return json.Number("7")
	case FieldBool:
		return true
	case FieldTime:
		return "2026-01-02T03:04:05Z"
	case FieldJSON:
		return []any{"Sample"}
	default:
		return "Sample-" + path
	}
}

// Every column resource in every shipped spec must rebuild a subset of the
// upstream document: each value at the path it came from, with the same value
// and the same JSON type. A spec that renames, flattens or re-types a value
// serves a document no client of the upstream can read.
func TestShippedSpecs_RebuildASubsetOfTheUpstreamDocument(t *testing.T) {
	for _, path := range shippedSpecs {
		spec, err := Load(path)
		require.NoError(t, err, path)
		for _, res := range spec.Resources {
			if res.Store != StoreColumns {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", spec.Name, res.Name), func(t *testing.T) {
				upstream := map[string]any{}
				for _, f := range res.Fields {
					if f.From != "" {
						setPath(upstream, f.From, sampleOf(f.Type, normalPath(f.From)))
					}
				}
				for _, k := range res.Keys {
					if k.Served() {
						setPath(upstream, k.From, sampleOf(cmp.Or(k.Type, FieldText), normalPath(k.From)))
					}
				}
				doc, err := decodeJSON(mustMarshal(t, upstream))
				require.NoError(t, err)

				row, err := absorb(res, doc)
				require.NoError(t, err)
				out, err := rebuild(res, row)
				require.NoError(t, err)

				var served any
				require.NoError(t, json.Unmarshal(mustMarshal(t, out), &served))
				var want any
				require.NoError(t, json.Unmarshal(mustMarshal(t, doc), &want))
				assertSubset(t, want, served, "$")
			})
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := marshalJSON(v)
	require.NoError(t, err)
	return b
}

// assertSubset requires every leaf of got to sit in want at the same path,
// with the same value and type.
func assertSubset(t *testing.T, want, got any, at string) {
	t.Helper()
	gm, ok := got.(map[string]any)
	if !ok {
		assert.Equal(t, want, got, "%s: the served value differs from the upstream's", at)
		return
	}
	wm, ok := want.(map[string]any)
	if !assert.True(t, ok, "%s: served an object where the upstream has %T", at, want) {
		return
	}
	for k, gv := range gm {
		wv, ok := wm[k]
		if !assert.True(t, ok, "%s.%s: served a member the upstream never sent (have %s)", at, k, strings.Join(keysOf(wm), ", ")) {
			continue
		}
		assertSubset(t, wv, gv, at+"."+k)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
