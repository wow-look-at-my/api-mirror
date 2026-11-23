package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagSpec adds a paginated document list of tags and a push that patches a
// moved tag in every stored page.
func tagSpec(s *Spec) {
	s.Resources = append(s.Resources, &Resource{
		Name:   "tags",
		Store:  StoreDocument,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "owner"}, {Name: "repo"}, {Name: "per_page"}, {Name: "page"}},
		Reveal: &Reveal{Public: "true"},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "GET", Path: "/repos/{owner}/{repo}/tags", Resource: "tags", List: true,
		Query: []QueryParam{
			{Name: "per_page", Type: FieldInt, Default: "30", Min: 1, Max: 100, Key: true},
			{Name: "page", Type: FieldInt, Default: "1", Min: 1, Max: 100, Key: true},
		},
		Absorb: []int{http.StatusNotFound},
	})
	s.Events = &Events{
		Path: "/webhook", Secret: testSecret, SignatureHeader: "X-Hub-Signature-256", TypeHeader: "X-Event",
		List: []*Event{{
			Type: "push", Resource: "tags", Subject: "tags:{{ .payload.repository.name }}", Clock: "repository.pushed_at",
			Keys: []Set{{Field: "owner", From: "repository.owner.login"}, {Field: "repo", From: "repository.name"}},
			Patch: &Patch{
				Match: "name",
				Expr:  `{{ trimPrefix "refs/tags/" .payload.ref }}`,
				Sets:  []Set{{Field: "commit.sha", From: "after"}},
			},
		}},
	}
}

func tagUpstream(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Query().Get("page") {
		case "1", "":
			w.Write([]byte(`[{"name":"v1","commit":{"sha":"a1"}},{"name":"v2","commit":{"sha":"a2"}}]`))
		case "2":
			w.Write([]byte(`[{"name":"v3","commit":{"sha":"a3"}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
}

func tagPush(tag, after string, pushedAt int64) map[string]any {
	return map[string]any{
		"ref": "refs/tags/" + tag, "before": "a1", "after": after,
		"repository": map[string]any{"name": "tool", "owner": map[string]any{"login": "acme"}, "pushed_at": pushedAt},
	}
}

func tagShas(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var items []map[string]any
	require.NoError(t, json.Unmarshal(body, &items))
	out := map[string]string{}
	for _, item := range items {
		out[item["name"].(string)] = item["commit"].(map[string]any)["sha"].(string)
	}
	return out
}

func TestAPatchRewritesTheItemInEveryPageThatListsIt(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, tagUpstream(&calls), tagSpec)
	for _, path := range []string{"/repos/acme/tool/tags", "/repos/acme/tool/tags?per_page=1&page=1", "/repos/acme/tool/tags?page=2", "/repos/acme/tool/tags?page=3"} {
		get(t, e, path)
	}
	require.EqualValues(t, 4, calls.Load())

	w := deliver(t, ingestFor(t, e), "push", tagPush("v1", "b1", 100))
	assert.Equal(t, string(DeliveryApplied), w.Header().Get(dispositionHeader))

	first := get(t, e, "/repos/acme/tool/tags")
	require.Equal(t, http.StatusOK, first.Code)
	assert.Equal(t, string(OutcomeHit), first.Header().Get("X-Mirror-Cache"))
	assert.Equal(t, map[string]string{"v1": "b1", "v2": "a2"}, tagShas(t, first.Body.Bytes()))
	small := get(t, e, "/repos/acme/tool/tags?per_page=1&page=1")
	assert.Equal(t, "b1", tagShas(t, small.Body.Bytes())["v1"], "every page that lists the tag carries the new sha")
	second := get(t, e, "/repos/acme/tool/tags?page=2")
	assert.Equal(t, map[string]string{"v3": "a3"}, tagShas(t, second.Body.Bytes()), "a page without the tag is untouched")
	assert.Equal(t, http.StatusNotFound, get(t, e, "/repos/acme/tool/tags?page=3").Code, "a stored refusal is not a page and stays")
	assert.EqualValues(t, 4, calls.Load(), "nothing was fetched to learn what the delivery stated")
}

func TestAPatchKeepsThePayloadsJSONType(t *testing.T) {
	payload := map[string]any{"forced": true, "size": json.Number("7"), "commit": map[string]any{"sha": "b2"}}
	p := &Patch{Match: "name", Sets: []Set{
		{Field: "forced", From: "forced"},
		{Field: "size", From: "size"},
		{Field: "commit", From: "commit"},
		{Field: "label", Expr: `{{ .payload.commit.sha }}!`},
	}}
	values := make([]any, len(p.Sets))
	for n, st := range p.Sets {
		v, err := patchValue(st, payload)
		require.NoError(t, err)
		values[n] = v
	}
	body, ok := patchPage(`[{"name":"v1","forced":false,"size":1,"commit":{"sha":"a1"}}]`, p, "v1", values)
	require.True(t, ok)
	assert.JSONEq(t, `[{"name":"v1","forced":true,"size":7,"commit":{"sha":"b2"},"label":"b2!"}]`, body)
}

func TestAPatchThatMatchesNothingChangesNothing(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, tagUpstream(&calls), tagSpec)
	before := get(t, e, "/repos/acme/tool/tags").Body.String()

	w := deliver(t, ingestFor(t, e), "push", tagPush("v9", "b9", 100))
	assert.Equal(t, string(DeliveryIgnored), w.Header().Get(dispositionHeader))
	assert.Equal(t, before, get(t, e, "/repos/acme/tool/tags").Body.String())
	assert.EqualValues(t, 1, calls.Load())
}

func TestAnUnreadablePageDropsThePrefix(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, tagUpstream(&calls), tagSpec)
	get(t, e, "/repos/acme/tool/tags")
	get(t, e, "/repos/acme/tool/tags?page=2")
	res, ok := e.store.Resource("tags")
	require.True(t, ok)
	require.NoError(t, e.store.Put(context.Background(), res,
		Row{"owner": "acme", "repo": "tool", "per_page": "30", "page": "2", "document": `{"not":"a page"}`}, time.Now()))

	w := deliver(t, ingestFor(t, e), "push", tagPush("v1", "b1", 100))
	assert.Equal(t, string(DeliveryInvalidated), w.Header().Get(dispositionHeader))

	rows, err := e.store.List(context.Background(), res, map[string]string{"owner": "acme", "repo": "tool"})
	require.NoError(t, err)
	assert.Empty(t, rows, "a page that cannot be patched drops every page under the key")
	assert.Equal(t, map[string]string{"v1": "a1", "v2": "a2"}, tagShas(t, get(t, e, "/repos/acme/tool/tags").Body.Bytes()))
	assert.EqualValues(t, 3, calls.Load(), "the next read refetches")
}

func TestAPatchStatingNoValueDropsThePrefix(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, tagUpstream(&calls), tagSpec)
	get(t, e, "/repos/acme/tool/tags")
	push := tagPush("v1", "b1", 100)
	delete(push, "after")

	w := deliver(t, ingestFor(t, e), "push", push)
	assert.Equal(t, string(DeliveryInvalidated), w.Header().Get(dispositionHeader),
		"a payload without the new value cannot correct the page, and the page may be stale")
}

func TestAPatchOlderThanAnAppliedOneIsRefused(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, tagUpstream(&calls), tagSpec)
	get(t, e, "/repos/acme/tool/tags")
	in := ingestFor(t, e)

	// Recent clocks, because the watermark sweep forgets a month-old moment.
	now := time.Now().Unix()
	deliver(t, in, "push", tagPush("v1", "new", now))
	w := deliver(t, in, "push", tagPush("v1", "old", now-60))
	assert.Equal(t, string(DeliverySuperseded), w.Header().Get(dispositionHeader))
	assert.Equal(t, "new", tagShas(t, get(t, e, "/repos/acme/tool/tags").Body.Bytes())["v1"])
}

func TestValidatePatchEvents(t *testing.T) {
	base := func() *Spec {
		s := testSpec("http://example.invalid")
		tagSpec(s)
		return s
	}
	require.NoError(t, base().validate())

	cases := []struct {
		name string
		bend func(s *Spec)
		want string
	}{
		{"not a document", func(s *Spec) {
			res := s.Resources[len(s.Resources)-1]
			res.Store = StoreColumns
			res.Fields = []Field{{Name: "name", Type: FieldText, From: "name"}}
		}, "not a document list"},
		{"not a list", func(s *Spec) { s.Routes[len(s.Routes)-1].List = false }, "not a document list"},
		{"no match", func(s *Spec) { s.Events.List[0].Patch.Match = "" }, "needs match="},
		{"no value", func(s *Spec) { s.Events.List[0].Patch.Expr = "" }, "needs from="},
		{"two values", func(s *Spec) { s.Events.List[0].Patch.From = "ref" }, "not both"},
		{"no sets", func(s *Spec) { s.Events.List[0].Patch.Sets = nil }, "sets nothing"},
		{"set without source", func(s *Spec) { s.Events.List[0].Patch.Sets[0].From = "" }, "not both and not neither"},
		{"beside invalidate", func(s *Spec) { s.Events.List[0].Invalidate = &Invalidate{Reason: "r"} }, "two writers"},
		{"no key", func(s *Spec) { s.Events.List[0].Keys = nil }, "naming the parent"},
		{"absorbs superseded", func(s *Spec) { s.Events.List[0].AbsorbWhenSuperseded = true }, "absorb-when-superseded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base()
			c.bend(s)
			assert.ErrorContains(t, s.validate(), c.want)
		})
	}
}

// The GitHub sample patches a moved tag and drops the pages for a created or
// deleted one. Exactly one of its tag push events takes each delivery.
func TestGitHubSpecPatchesAMovedTag(t *testing.T) {
	spec, err := Load(filepath.Join(repoRoot, "samples", "github", "github.xml"))
	require.NoError(t, err)
	var patch, drop *Event
	for _, ev := range spec.Events.List {
		if ev.Type != "push" || ev.Resource != "tags_list" {
			continue
		}
		if ev.Patch != nil {
			patch = ev
		} else {
			drop = ev
		}
	}
	require.NotNil(t, patch)
	require.NotNil(t, drop)
	require.NotNil(t, drop.Invalidate)
	assert.Equal(t, "name", patch.Patch.Match)
	assert.Equal(t, []Set{{Field: "commit.sha", From: "after"}}, patch.Patch.Sets)

	const zero = "0000000000000000000000000000000000000000"
	cases := []struct {
		name          string
		before, after string
		ref           string
		patched       bool
		dropped       bool
	}{
		{"moved", "a1", "b1", "refs/tags/v1", true, false},
		{"created", zero, "b1", "refs/tags/v1", false, true},
		{"deleted", "a1", zero, "refs/tags/v1", false, true},
		{"a branch", "a1", "b1", "refs/heads/main", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := map[string]any{"ref": c.ref, "before": c.before, "after": c.after}
			got, err := handles(patch, payload)
			require.NoError(t, err)
			assert.Equal(t, c.patched, got)
			got, err = handles(drop, payload)
			require.NoError(t, err)
			assert.Equal(t, c.dropped, got)
		})
	}
}
