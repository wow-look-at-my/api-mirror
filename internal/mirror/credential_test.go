package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialResource is a resource keyed by the caller's own credential
// fingerprint: one row per token, self-gated instead of probed.
func credentialResource() *Resource {
	return &Resource{
		Name:   "identity",
		Store:  StoreColumns,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "token_fp", Credential: true}},
		Fields: []Field{{Name: "login", Type: FieldText, From: "login"}},
		Reveal: &Reveal{Credential: true},
	}
}

func withCredentialResource(rt *Route) func(*Spec) {
	return func(s *Spec) {
		s.Upstream.Forward = append(s.Upstream.Forward, "Authorization")
		s.Resources = append(s.Resources, credentialResource())
		s.Routes = append(s.Routes, rt)
	}
}

func TestCredentialResource_OneRowPerCaller(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"` + r.Header.Get("Authorization") + `"}`))
	}), withCredentialResource(&Route{Method: "GET", Path: "/user", Resource: "identity"}))

	getAs := func(auth string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/user", nil)
		req.Header.Set("Authorization", auth)
		e.ServeHTTP(rec, req)
		return rec
	}

	first := getAs("token alice")
	assert.Equal(t, http.StatusOK, first.Code)
	assert.Contains(t, first.Body.String(), `"login":"token alice"`)

	second := getAs("token bob")
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Contains(t, second.Body.String(), `"login":"token bob"`)

	// Two different credentials are two different rows, so both fetched.
	assert.EqualValues(t, 2, calls.Load())

	// The same credential replays its own row without a second fetch.
	again := getAs("token alice")
	assert.Contains(t, again.Body.String(), `"login":"token alice"`)
	assert.EqualValues(t, 2, calls.Load())
}

func TestCredentialResource_NoCredentialIsPassthrough(t *testing.T) {
	// A passthrough genuinely forwards to the upstream, so the fake upstream
	// here must answer rather than fail the test on being asked.
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }),
		withCredentialResource(&Route{Method: "GET", Path: "/user", Resource: "identity"}))

	rec := get(t, e, "/user")
	assert.Equal(t, "passthrough", rec.Header().Get("X-Mirror-Cache"))
	assert.Equal(t, string(PassNoIdentity), rec.Header().Get("X-Mirror-Passthrough-Reason"))
}

func TestAllow_CredentialResourceNeverProbes(t *testing.T) {
	res := credentialResource()
	rv := NewRevealer(nil, mustUpstreamer(t, unreachable(t)), map[string]any{})
	verdict, err := rv.Allow(context.Background(), "token:abc", res, map[string]string{"token_fp": "abc"}, http.Header{})
	require.NoError(t, err)
	assert.True(t, verdict.Allowed)
}

// mustUpstreamer wires an Upstreamer to a fake upstream, failing the test if
// building it errors.
func mustUpstreamer(t *testing.T, h http.HandlerFunc) *Upstreamer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	up, err := NewUpstreamer(&Spec{Upstream: Upstream{Base: srv.URL}}, map[string]any{}, nil)
	require.NoError(t, err)
	return up
}

func TestValidate_CredentialResource(t *testing.T) {
	base := func() *Resource { return credentialResource() }

	t.Run("credential reveal needs a credential key", func(t *testing.T) {
		r := base()
		r.Keys = []Key{{Name: "token_fp", From: "token_fp"}}
		err := r.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needs a <key credential")
	})

	t.Run("credential key rejects a from path", func(t *testing.T) {
		r := base()
		r.Keys = []Key{{Name: "token_fp", Credential: true, From: "token_fp"}}
		err := r.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pick one")
	})

	t.Run("credential reveal rejects a public predicate", func(t *testing.T) {
		r := base()
		r.Reveal = &Reveal{Credential: true, Public: "true"}
		err := r.validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dead code")
	})

	t.Run("valid credential resource passes", func(t *testing.T) {
		require.NoError(t, base().validate())
	})
}

func TestValidateRoute_CredentialKeyNeedsNoParam(t *testing.T) {
	resources := map[string]*Resource{"identity": credentialResource()}
	rt := &Route{Method: "GET", Path: "/user", Resource: "identity"}
	assert.NoError(t, rt.validate(resources))
}

// TestPurge_SuccessfulWriteDropsTheCachedRow proves a write is forwarded
// verbatim to the upstream, and that only a confirmed 2xx clears the row it
// changed -- a rejected write must not blow away a still-correct cache entry.
func TestPurge_SuccessfulWriteDropsTheCachedRow(t *testing.T) {
	var method atomic.Value
	nextStatus := atomic.Int32{}
	nextStatus.Store(http.StatusOK)
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method.Store(r.Method)
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"7","title":"hello","count":3,"live":true,"seen":"2024-01-01T00:00:00Z"}`))
			return
		}
		w.WriteHeader(int(nextStatus.Load()))
	}), func(s *Spec) {
		s.Purges = append(s.Purges, &Purge{Method: "DELETE", Path: "/widgets/{id}", Resource: "widget"})
	})

	// Warm the cache.
	rec := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, rec.Code)
	row, err := e.store.Get(context.Background(), mustResource(t, e, "widget"), map[string]string{"id": "7"})
	require.NoError(t, err)
	require.NotNil(t, row)

	// A rejected delete must not touch the cached row.
	nextStatus.Store(http.StatusForbidden)
	del := httptest.NewRecorder()
	e.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/widgets/7", nil))
	assert.Equal(t, http.StatusForbidden, del.Code)
	row, err = e.store.Get(context.Background(), mustResource(t, e, "widget"), map[string]string{"id": "7"})
	require.NoError(t, err)
	assert.NotNil(t, row)

	// A confirmed delete drops it.
	nextStatus.Store(http.StatusNoContent)
	del = httptest.NewRecorder()
	e.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/widgets/7", nil))
	assert.Equal(t, http.StatusNoContent, del.Code)
	assert.Equal(t, http.MethodDelete, method.Load())
	row, err = e.store.Get(context.Background(), mustResource(t, e, "widget"), map[string]string{"id": "7"})
	require.NoError(t, err)
	assert.Nil(t, row)
}

func mustResource(t *testing.T, e *Engine, name string) *Resource {
	t.Helper()
	res, ok := e.store.Resource(name)
	require.True(t, ok)
	return res
}

func TestValidatePurge(t *testing.T) {
	resources := map[string]*Resource{"widget": {
		Name: "widget", Store: StoreColumns, Keys: []Key{{Name: "id"}},
		Fields: []Field{{Name: "title", Type: FieldText, From: "title"}},
		Reveal: &Reveal{Public: "true"},
	}}

	t.Run("read method rejected", func(t *testing.T) {
		p := &Purge{Method: "GET", Path: "/widgets/{id}", Resource: "widget"}
		require.Error(t, p.validate(resources))
	})

	t.Run("unknown resource rejected", func(t *testing.T) {
		p := &Purge{Method: "DELETE", Path: "/widgets/{id}", Resource: "nope"}
		require.Error(t, p.validate(resources))
	})

	t.Run("missing key param rejected", func(t *testing.T) {
		p := &Purge{Method: "DELETE", Path: "/widgets", Resource: "widget"}
		require.Error(t, p.validate(resources))
	})

	t.Run("valid purge passes", func(t *testing.T) {
		p := &Purge{Method: "DELETE", Path: "/widgets/{id}", Resource: "widget"}
		require.NoError(t, p.validate(resources))
	})
}

func TestLoad_CredentialXML(t *testing.T) {
	xml := `<mirror name="cred-test">
	<upstream base="https://api.example.com"/>
	<resource name="identity">
		<key name="token_fp" credential="true"/>
		<field name="login">login</field>
		<reveal><credential/></reveal>
	</resource>
	<route method="GET" path="/user" resource="identity"/>
</mirror>`
	spec, err := ParseSpec([]byte(xml))
	require.NoError(t, err)
	require.NoError(t, spec.validate())
	require.Len(t, spec.Resources, 1)
	require.Len(t, spec.Resources[0].Keys, 1)
	assert.True(t, spec.Resources[0].Keys[0].Credential)
	assert.True(t, spec.Resources[0].Reveal.Credential)
	openStore := func() (*Store, error) {
		return Open(context.Background(), filepath.Join(t.TempDir(), "cred.db"), spec)
	}
	store, err := openStore()
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
}
