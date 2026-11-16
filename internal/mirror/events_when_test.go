package mirror

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandles_AnActionListTakesOnlyTheActionsItNames(t *testing.T) {
	ev := &Event{Type: "repository", Actions: []string{"deleted", "renamed"}}

	ok, err := handles(ev, map[string]any{"action": "renamed"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = handles(ev, map[string]any{"action": "edited"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHandles_AWhenPredicateSplitsOneTypeByPayload(t *testing.T) {
	tip := &Event{Type: "push", When: `{{ and (hasPrefix "refs/heads/" .payload.ref) (not .payload.deleted) }}`}
	gone := &Event{Type: "push", When: `{{ and (hasPrefix "refs/heads/" .payload.ref) .payload.deleted }}`}

	cases := []struct {
		name    string
		payload map[string]any
		tip     bool
		gone    bool
	}{
		{"a branch push moves the tip", map[string]any{"ref": "refs/heads/main", "deleted": false}, true, false},
		{"a deleting push drops the row", map[string]any{"ref": "refs/heads/main", "deleted": true}, false, true},
		{"a tag push is neither", map[string]any{"ref": "refs/tags/v1", "deleted": false}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := handles(tip, c.payload)
			require.NoError(t, err)
			assert.Equal(t, c.tip, ok)
			ok, err = handles(gone, c.payload)
			require.NoError(t, err)
			assert.Equal(t, c.gone, ok)
		})
	}
}

func TestValidate_AKeylessInvalidateMustSayAll(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Keys = nil
	s.Events.List[0].Sets = nil
	s.Events.List[0].Invalidate = &Invalidate{Reason: "r"}
	assert.ErrorContains(t, s.validate(), "invalidates every row")

	s.Events.List[0].Invalidate.All = true
	assert.NoError(t, s.validate())
}

func TestValidate_AnInvalidateAllRefusesAKey(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Sets = nil
	s.Events.List[0].Keys = []Set{{Field: "owner", From: "repository.owner.login"}}
	s.Events.List[0].Invalidate = &Invalidate{Reason: "r", All: true}
	assert.ErrorContains(t, s.validate(), "would narrow nothing")
}

func TestHandles_APredicateThatCannotRenderFails(t *testing.T) {
	ev := &Event{Type: "push", When: `{{ hasPrefix }}`}
	_, err := handles(ev, map[string]any{})
	assert.Error(t, err)
}

func TestWhenFalseSkipsTheEvent(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[0].When = `{{ eq .payload.action "publicized" }}`
	in, store := newIngest(t, spec)

	skipped := repoDelivery("acme", "widget", clockEarly, map[string]any{"visibility": "public"})
	skipped["action"] = "edited"
	w := deliver(t, in, "repository", skipped)
	assert.Equal(t, string(DeliveryIgnored), w.Header().Get(dispositionHeader))
	assert.Nil(t, repoRow(t, store, "acme", "widget"), "a declined event writes nothing")

	applied := repoDelivery("acme", "widget", clockLate, map[string]any{"visibility": "public"})
	applied["action"] = "publicized"
	w = deliver(t, in, "repository", applied)
	assert.Equal(t, string(DeliveryApplied), w.Header().Get(dispositionHeader))
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

func TestInvalidateEveryDropsTheWholeResource(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[1].Keys = nil
	spec.Events.List[1].Invalidate.Every = true
	require.NoError(t, spec.validate())
	in, store := newIngest(t, spec)

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly, map[string]any{"visibility": "public"}))
	deliver(t, in, "repository", repoDelivery("other", "thing", clockEarly, map[string]any{"visibility": "public"}))

	w := deliver(t, in, "repository_deleted", repoDelivery("acme", "widget", clockLate, nil))
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
	assert.Nil(t, repoRow(t, store, "other", "thing"), "every=\"true\" reaches rows the payload does not name")
}

// tipSpec adds a second event on the same type and resource, which drops
// the stored row only when the delivery disagrees with its visibility.
func tipSpec() *Spec {
	spec := ingestSpec()
	spec.Events.List[0].When = `{{ ne .payload.action "check" }}`
	spec.Events.List = append(spec.Events.List, &Event{
		Type:     "repository",
		Resource: "repo",
		When:     `{{ eq .payload.action "check" }}`,
		Subject:  "check:{{ .payload.repository.full_name }}",
		Clock:    "repository.updated_at",
		Keys: []Set{
			{Field: "owner", From: "repository.owner.login"},
			{Field: "name", From: "repository.name"},
		},
		Invalidate: &Invalidate{Reason: "a disagreement means a lost delivery", Field: "visibility", Differs: "repository.visibility"},
	})
	return spec
}

func TestConditionalInvalidateDropsOnlyAContradictedRow(t *testing.T) {
	spec := tipSpec()
	require.NoError(t, spec.validate())
	in, store := newIngest(t, spec)
	deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly, map[string]any{"visibility": "public"}))

	agree := repoDelivery("acme", "widget", clockLate, map[string]any{"visibility": "public"})
	agree["action"] = "check"
	w := deliver(t, in, "repository", agree)
	assert.Equal(t, string(DeliveryIgnored), w.Header().Get(dispositionHeader))
	assert.NotNil(t, repoRow(t, store, "acme", "widget"), "an agreeing delivery keeps the row")

	disagree := repoDelivery("acme", "widget", clockLate+1, map[string]any{"visibility": "private"})
	disagree["action"] = "check"
	w = deliver(t, in, "repository", disagree)
	assert.Equal(t, string(DeliveryInvalidated), w.Header().Get(dispositionHeader))
	assert.Nil(t, repoRow(t, store, "acme", "widget"), "a contradicted row is dropped so the next read refetches")
}

func TestValidateRefusesAnUngatedRepeat(t *testing.T) {
	spec := tipSpec()
	spec.Events.List[0].When = ""
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs when= on every declaration")
}

func TestValidateConditionalInvalidateShape(t *testing.T) {
	spec := tipSpec()
	spec.Events.List[2].Invalidate.Differs = ""
	require.Error(t, spec.validate())

	spec = tipSpec()
	spec.Events.List[2].Invalidate.Field = "nope"
	require.Error(t, spec.validate())

	spec = tipSpec()
	spec.Events.List[2].Keys = spec.Events.List[2].Keys[:1]
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compares one row")
}

func TestValidateRefusesEveryWithKeys(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[1].Invalidate.Every = true
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would be ignored")
}
