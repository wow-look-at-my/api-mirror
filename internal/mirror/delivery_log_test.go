package mirror

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryLog_KeepsTheNewestFirstAndCountsWhatItDropped(t *testing.T) {
	var l deliveryLog
	for n := range deliveryLogSize + 3 {
		l.add(DeliveryRecord{Type: "push", Status: n})
	}
	got, dropped := l.recent()
	require.Len(t, got, deliveryLogSize)
	assert.Equal(t, 3, dropped)
	assert.Equal(t, deliveryLogSize+2, got[0].Status, "newest first")
	assert.Equal(t, 3, got[len(got)-1].Status, "the three oldest were overwritten")
}

func TestOrderingStats_CountLagAndReorderedBatches(t *testing.T) {
	var s orderingStats
	now := time.Now()
	ordered := &Event{Type: "push"}
	s.applied(&Delivery{Event: ordered, At: now.Add(-2 * time.Second)}, DeliveryApplied, now)
	s.applied(&Delivery{Event: ordered, At: now.Add(-4 * time.Second)}, DeliverySuperseded, now)
	s.applied(&Delivery{Event: &Event{Type: "meta", Unordered: true}}, DeliveryApplied, now)
	s.batch([]*Delivery{{At: now}, {At: now.Add(-time.Second)}})
	s.batch([]*Delivery{{At: now.Add(-time.Second)}, {At: now}})

	v := s.view(2 * time.Second)
	assert.Equal(t, 2, v.Ordered)
	assert.Equal(t, 1, v.Unordered)
	assert.Equal(t, 1, v.Superseded)
	assert.InDelta(t, 3.0, v.MeanLagSeconds, 0.01)
	assert.InDelta(t, 4.0, v.WorstLagSeconds, 0.01)
	assert.Equal(t, 4, v.Held)
	assert.Equal(t, 1, v.Reordered, "only the batch that arrived out of order")
}

func TestMissingSubscriptions_ReportsDeclaredTypesTheUpstreamDoesNotSend(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/app", r.URL.Path)
		requireAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
		body, err := json.Marshal(map[string]any{"events": []string{"push"}})
		require.NoError(t, err)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	spec := &Spec{
		Name:     "subs",
		Upstream: Upstream{Base: srv.URL, App: &App{ID: "42", Key: pemText}},
		Events: &Events{
			List: []*Event{
				{Type: "push", Resource: "branch"},
				{Type: "pull_request", Resource: "pull"},
				{Type: "pull_request", Resource: "branch"},
				{Type: "installation", Resource: "install_token"},
			},
			Subscriptions: &EventSubscriptions{Path: "/app", Field: "events", Always: []string{"installation"}},
		},
	}
	up, err := NewUpstreamer(spec, map[string]any{}, nil)
	require.NoError(t, err)
	e := &Engine{spec: spec, up: up, vars: map[string]any{}}

	missing, err := e.missingSubscriptions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []MissingSubscription{{Type: "pull_request", Resources: []string{"pull", "branch"}}}, missing)
}

func TestMissingSubscriptions_WithoutAnAppClaimsNothing(t *testing.T) {
	e := &Engine{spec: &Spec{Events: &Events{Subscriptions: &EventSubscriptions{Path: "/app", Field: "events"}}}}
	missing, err := e.missingSubscriptions(context.Background())
	require.NoError(t, err)
	assert.Nil(t, missing)
}
