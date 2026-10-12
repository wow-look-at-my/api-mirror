package mirror

import (
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldSpec adds a redirect resource and a text resource.
func heldSpec(s *Spec) {
	s.Resources = append(s.Resources,
		&Resource{
			Name: "logs", Store: StoreRedirect, TTL: time.Hour,
			Keys:   []Key{{Name: "id"}},
			Reveal: &Reveal{Public: "true"},
			Redirect: &Redirect{
				Margin:   30 * time.Second,
				Unsigned: time.Hour,
				Expires:  []ExpiryParam{{Param: "se", Format: "time"}, {Param: "X-Amz-Expires", Format: "seconds", Since: "X-Amz-Date"}},
			},
		},
		&Resource{
			Name: "diff", Store: StoreText, TTL: time.Hour,
			Keys:   []Key{{Name: "id"}},
			Reveal: &Reveal{Public: "true"},
		},
	)
	s.Routes = append(s.Routes,
		&Route{Method: "GET", Path: "/logs/{id}", Resource: "logs", Absorb: []int{410}},
		&Route{Method: "GET", Path: "/widgets/{id}", Resource: "diff", Accept: []string{"application/vnd.example.diff"}},
	)
	s.Routes[0].Accept = []string{"application/json"}
}

func TestARedirectIsHeldUntilItsSignatureExpires(t *testing.T) {
	var calls atomic.Int32
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	e, srv := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/blob" {
			t.Errorf("the fetch followed the redirect it was meant to keep")
			return
		}
		w.Header().Set("Location", "/blob?se="+url.QueryEscape(expiry)+"&sig=abc")
		w.WriteHeader(http.StatusFound)
	}), heldSpec)

	first := get(t, e, "/logs/9")
	require.Equal(t, http.StatusFound, first.Code, first.Body.String())
	assert.Equal(t, srv.URL+"/blob?se="+url.QueryEscape(expiry)+"&sig=abc", first.Header().Get("Location"),
		"a relative Location resolves against the upstream base")
	assert.Equal(t, string(OutcomeMiss), first.Header().Get("X-Mirror-Cache"))

	second := get(t, e, "/logs/9")
	assert.Equal(t, http.StatusFound, second.Code)
	assert.Equal(t, first.Header().Get("Location"), second.Header().Get("Location"))
	assert.Equal(t, string(OutcomeHit), second.Header().Get("X-Mirror-Cache"))
	assert.EqualValues(t, 1, calls.Load())
}

func TestARedirectAboutToExpireIsRelayedAndNotHeld(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		soon := time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339)
		w.Header().Set("Location", "https://blob.example/x?se="+url.QueryEscape(soon))
		w.WriteHeader(http.StatusFound)
	}), heldSpec)

	for range 2 {
		rec := get(t, e, "/logs/9")
		assert.Equal(t, http.StatusFound, rec.Code, "the caller still gets the redirect")
		assert.Contains(t, rec.Header().Get("Location"), "blob.example")
	}
	assert.EqualValues(t, 2, calls.Load(), "a URL inside its margin is nobody's to keep")
}

func TestAnAbsorbedRefusalOnARedirectRouteIsKept(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"message":"logs expired"}`))
	}), heldSpec)

	assert.Equal(t, http.StatusGone, get(t, e, "/logs/9").Code)
	rec := get(t, e, "/logs/9")
	assert.Equal(t, http.StatusGone, rec.Code)
	assert.Contains(t, rec.Body.String(), "logs expired")
	assert.EqualValues(t, 1, calls.Load())
}

func TestATextAnswerKeepsItsMediaType(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") != "application/vnd.example.diff" {
			w.Write([]byte(`{"id":"7","title":"json"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.example.diff; charset=utf-8")
		w.Write([]byte("diff --git a/x b/x\n"))
	}), heldSpec)

	for _, want := range []Outcome{OutcomeMiss, OutcomeHit} {
		rec := getAccept(t, e, "/widgets/7", "application/vnd.example.diff")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "diff --git a/x b/x\n", rec.Body.String())
		assert.Equal(t, "application/vnd.example.diff; charset=utf-8", rec.Header().Get("Content-Type"))
		assert.Equal(t, string(want), rec.Header().Get("X-Mirror-Cache"))
	}
	json := getAccept(t, e, "/widgets/7", "application/json")
	assert.Contains(t, json.Body.String(), `"title":"json"`, "the JSON route on the same path still answers JSON")
	assert.EqualValues(t, 2, calls.Load())
}

func TestRedirectLifetime(t *testing.T) {
	now := time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)
	rd := &Redirect{Margin: time.Minute, Expires: []ExpiryParam{{Param: "X-Amz-Expires", Format: "seconds", Since: "X-Amz-Date"}}}

	loc, _ := url.Parse("https://s3.example/x?X-Amz-Date=20261012T035900Z&X-Amz-Expires=600")
	ttl, ok := rd.lifetime(loc, now)
	require.True(t, ok)
	assert.Equal(t, 8*time.Minute, ttl, "signed at 03:59 for ten minutes, less a minute of margin")

	signed, _ := url.Parse("https://codeload.example/x?token=abc")
	_, ok = rd.lifetime(signed, now)
	assert.False(t, ok, "a signature the rule cannot date is never kept without a fallback")

	rd.Fallback = time.Minute
	ttl, ok = rd.lifetime(signed, now)
	require.True(t, ok)
	assert.Equal(t, time.Minute, ttl)
}

func TestValidateHeldResources(t *testing.T) {
	s := testSpec("http://example.invalid")
	heldSpec(s)
	require.NoError(t, s.validate())

	s = testSpec("http://example.invalid")
	heldSpec(s)
	s.Resources[2].Redirect = nil
	assert.ErrorContains(t, s.validate(), "declares no <redirect>")

	s = testSpec("http://example.invalid")
	heldSpec(s)
	s.Routes[3].Accept = nil
	assert.ErrorContains(t, s.validate(), "needs <accept>")

	s = testSpec("http://example.invalid")
	heldSpec(s)
	s.Routes[2].List = true
	assert.ErrorContains(t, s.validate(), "one opaque row")
}
