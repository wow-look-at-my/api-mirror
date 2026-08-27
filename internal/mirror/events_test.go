package mirror

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ingestSpec is the spec every ingest test opens against.
func ingestSpec() *Spec {
	return &Spec{
		Name:     "test",
		Upstream: Upstream{Base: "https://api.example.com"},
		Resources: []*Resource{{
			Name:  "repo",
			Store: StoreColumns,
			TTL:   time.Hour,
			Keys: []Key{
				{Name: "owner", From: "repository.owner.login", Fold: true},
				{Name: "name", From: "repository.name", Fold: true},
			},
			Fields: []Field{
				{Name: "visibility", Type: FieldText, From: "visibility"},
				{Name: "stars", Type: FieldInt, From: "stargazers_count"},
				{Name: "topic", Type: FieldText, From: "topic"},
			},
			Reveal: &Reveal{Public: `{{ eq .row.visibility "public" }}`},
		}},
		Routes: []*Route{{Method: "GET", Path: "/repos/{owner}/{name}", Resource: "repo"}},
		Events: &Events{
			Path:            "/webhook",
			Secret:          testSecret,
			SignatureHeader: "X-Hub-Signature-256",
			TypeHeader:      "X-Event",
			List: []*Event{{
				Type:     "repository",
				Resource: "repo",
				Subject:  "{{ .payload.repository.full_name }}",
				Clock:    "repository.updated_at",
				Keys: []Set{
					{Field: "owner", From: "repository.owner.login"},
					{Field: "name", From: "repository.name"},
				},
				Sets: []Set{
					{Field: "visibility", From: "repository.visibility"},
					{Field: "stars", From: "repository.stargazers_count"},
					// A cleared topic and an absent one are different answers
					{Field: "topic", From: "repository.topic", AllowNull: true},
				},
			}, {
				Type:     "repository_deleted",
				Resource: "repo",
				Subject:  "{{ .payload.repository.full_name }}",
				Clock:    "repository.updated_at",
				Keys: []Set{
					{Field: "owner", From: "repository.owner.login"},
					{Field: "name", From: "repository.name"},
				},
				Invalidate: &Invalidate{Reason: "the payload states the repo is gone, not its new state"},
			}},
		},
	}
}

const testSecret = "s3cret"

// A payload clock is a real moment. The watermark sweep forgets a subject
var (
	clockEarly = time.Now().Add(-time.Hour).Unix()
	clockLate  = time.Now().Unix()
)

func newIngest(t *testing.T, spec *Spec) (*Ingest, *Store) {
	t.Helper()
	store := openStore(t, dbPath(t), spec)
	in, err := NewIngest(spec, store, map[string]any{})
	require.NoError(t, err)
	return in, store
}

// repoDelivery builds one repository payload. The extra fields overlay the
// base, so a test states only what it is about.
func repoDelivery(owner, name string, updated int64, extra map[string]any) map[string]any {
	repo := map[string]any{
		"owner":      map[string]any{"login": owner},
		"name":       name,
		"full_name":  owner + "/" + name,
		"updated_at": updated,
	}
	for k, v := range extra {
		repo[k] = v
	}
	return map[string]any{"repository": repo}
}

// deliveryBody renders a payload the way a provider sends it: marshalled.
func deliveryBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := marshalJSON(v)
	require.NoError(t, err)
	return b
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postDelivery(t *testing.T, in *Ingest, typ string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Event", typ)
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	in.ServeHTTP(w, req)
	return w
}

// deliver posts a correctly signed delivery.
func deliver(t *testing.T, in *Ingest, typ string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body := deliveryBody(t, payload)
	return postDelivery(t, in, typ, body, signBody(testSecret, body))
}

func repoRow(t *testing.T, s *Store, owner, name string) Row {
	t.Helper()
	res, ok := s.Resource("repo")
	require.True(t, ok)
	row, err := s.Get(context.Background(), res, map[string]string{"owner": owner, "name": name})
	require.NoError(t, err)
	return row
}

func TestBadSignatureIsRefusedAndWritesNothing(t *testing.T) {
	in, store := newIngest(t, ingestSpec())
	body := deliveryBody(t, repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	w := postDelivery(t, in, "repository", body, signBody("wrong-key", body))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestMissingSignatureIsRefused(t *testing.T) {
	in, store := newIngest(t, ingestSpec())
	body := deliveryBody(t, repoDelivery("acme", "widget", clockEarly, nil))

	w := postDelivery(t, in, "repository", body, "")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestUnsetSecretRefusesEveryDelivery(t *testing.T) {
	spec := ingestSpec()
	spec.Events.Secret = ""
	in, store := newIngest(t, spec)
	body := deliveryBody(t, repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	// Even a body signed with the empty key is refused: with no secret there is
	w := postDelivery(t, in, "repository", body, signBody("", body))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestUnknownEventTypeIsIgnored(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	w := deliver(t, in, "gollum", repoDelivery("acme", "widget", clockEarly, nil))

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, string(DeliveryIgnored), w.Header().Get(dispositionHeader))
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestValidDeliveryApplies(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	w := deliver(t, in, "repository", repoDelivery("Acme", "Widget", clockEarly,
		map[string]any{"visibility": "public", "stargazers_count": 7}))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(DeliveryApplied), w.Header().Get(dispositionHeader))

	// The keys declare folding, so the row lands under the lower-cased spelling
	row := repoRow(t, store, "acme", "widget")
	require.NotNil(t, row)
	assert.Equal(t, "public", row["visibility"])
	assert.EqualValues(t, 7, row["stars"])
}

func TestOlderClockIsSupersededAndDoesNotOverwrite(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "private"}))
	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, string(DeliverySuperseded), w.Header().Get(dispositionHeader))
	assert.Equal(t, "private", repoRow(t, store, "acme", "widget")["visibility"])
}

func TestEqualClockApplies(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "private"}))
	// A clock is a second, and two distinct views of one subject land inside
	// the same second often. Refusing on equality drops the second one.
	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "public"}))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(DeliveryApplied), w.Header().Get(dispositionHeader))
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

func TestSupersededDeliveryStillAbsorbsWhenDeclared(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[0].AbsorbWhenSuperseded = true
	in, store := newIngest(t, spec)

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "private"}))
	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	// The verdict is still superseded: this view is not the newest one.
	assert.Equal(t, string(DeliverySuperseded), w.Header().Get(dispositionHeader))
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

func TestOmittedFieldIsNotBlanked(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public", "stargazers_count": 42}))
	// This view says nothing about the star count. A write that blanked it
	// would replace known state with nothing.
	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "private"}))

	row := repoRow(t, store, "acme", "widget")
	assert.Equal(t, "private", row["visibility"])
	assert.EqualValues(t, 42, row["stars"])
}

func TestAllowNullWritesTheStatedNull(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public", "topic": "caching"}))
	assert.Equal(t, "caching", repoRow(t, store, "acme", "widget")["topic"])

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate,
		map[string]any{"visibility": "public", "topic": nil}))

	assert.Nil(t, repoRow(t, store, "acme", "widget")["topic"])
}

func TestInvalidateDeletesTheRow(t *testing.T) {
	in, store := newIngest(t, ingestSpec())

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))
	require.NotNil(t, repoRow(t, store, "acme", "widget"))

	w := deliver(t, in, "repository_deleted", repoDelivery("acme", "widget", clockLate, nil))

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, string(DeliveryInvalidated), w.Header().Get(dispositionHeader))
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestWatermarkFailureStillApplies(t *testing.T) {
	in, store := newIngest(t, ingestSpec())
	// The ordering gate is now broken. A provider sends a delivery once, so the
	_, err := store.db.Exec(`DROP TABLE mirror_watermark`)
	require.NoError(t, err)

	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

func TestPayloadWithoutTheKeyIsAnError(t *testing.T) {
	in, store := newIngest(t, ingestSpec())
	payload := map[string]any{"repository": map[string]any{
		"full_name": "acme/widget", "updated_at": clockEarly, "visibility": "public",
	}}

	w := deliver(t, in, "repository", payload)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Nil(t, repoRow(t, store, "acme", "widget"))
}

func TestNonJSONBodyIsRefused(t *testing.T) {
	in, _ := newIngest(t, ingestSpec())
	body := []byte("not json")

	w := postDelivery(t, in, "repository", body, signBody(testSecret, body))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetIsNotADelivery(t *testing.T) {
	in, _ := newIngest(t, ingestSpec())
	w := httptest.NewRecorder()

	in.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webhook", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestWindowedDeliveryAnswersBeforeItApplies(t *testing.T) {
	spec := ingestSpec()
	spec.Events.ReorderWindow = 20 * time.Millisecond
	in, store := newIngest(t, spec)

	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	// The provider gives up in single-digit seconds, so the answer precedes the
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, "accepted", w.Header().Get(dispositionHeader))

	require.True(t, in.Drain(2*time.Second))
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

// A resource's from= describes the upstream DOCUMENT, and a delivery wraps that
// document in an envelope. The event's own <key> is what bridges the two.
func TestAnEventAddressesItsRowThroughItsOwnKeys(t *testing.T) {
	spec := ingestSpec()
	spec.Resources[0].Keys = []Key{
		{Name: "owner", From: "owner.login", Fold: true},
		{Name: "name", From: "name", Fold: true},
	}

	in, store := newIngest(t, spec)
	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public"}))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "public", repoRow(t, store, "acme", "widget")["visibility"])
}

// The response to a held delivery says only that it was taken, so counting that
// answer would report every failure as fine.
func TestAHeldDeliveryIsCountedByItsOutcome(t *testing.T) {
	spec := ingestSpec()
	spec.Events.ReorderWindow = 20 * time.Millisecond
	in, _ := newIngest(t, spec)

	// No repository.name, so the delivery cannot name the row it is about.
	w := deliver(t, in, "repository", map[string]any{
		"repository": map[string]any{"owner": map[string]any{"login": "acme"}, "updated_at": clockEarly},
	})
	require.Equal(t, string(DeliveryHeld), w.Header().Get(dispositionHeader))
	require.True(t, in.Drain(2*time.Second))

	stats := in.Stats()
	assert.Equal(t, 1, stats.Dispositions[DeliveryFailed])
	assert.Zero(t, stats.Dispositions[DeliveryHeld], "a delivery counted as taken is a failure nobody sees")
}

func TestReorderWindowAppliesOldestFirst(t *testing.T) {
	var mu sync.Mutex
	var order []int64
	r := NewReorderer(50*time.Millisecond, func(_ context.Context, d *Delivery) (DeliveryDisposition, error) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, d.At.Unix())
		return DeliveryApplied, nil
	})

	// Both deliveries are about one subject and land inside the window, newest
	r.Submit(&Delivery{ID: "newer", Subject: "acme/widget", At: time.Unix(2000, 0)})
	r.Submit(&Delivery{ID: "older", Subject: "acme/widget", At: time.Unix(1000, 0)})
	require.True(t, r.Drain(2*time.Second))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int64{1000, 2000}, order)
}
