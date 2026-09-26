package mirror

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestValidateRefusesEveryWithKeys(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[1].Invalidate.Every = true
	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would be ignored")
}
