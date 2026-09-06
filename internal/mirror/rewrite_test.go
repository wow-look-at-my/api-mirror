package mirror

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A client can insist on its own spelling of the same API. gh reads a host
// that is not github.com as GitHub Enterprise Server and puts every REST call
// under /api/v3, so without <rewrite> a mirror pointed at by GH_HOST answers
// nothing from cache and forwards the whole session.
func TestRewrite_APrefixedPathReachesTheSameRoute(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/widgets/7", r.URL.Path,
			"the upstream is asked in its own spelling, never the client's")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"7","title":"hello","count":3,"live":true,"seen":"2024-01-01T00:00:00Z"}`))
	}), func(s *Spec) {
		s.Rewrites = append(s.Rewrites, &Rewrite{From: "/api/v3"})
	})

	first := get(t, e, "/api/v3/widgets/7")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, string(OutcomeMiss), first.Header().Get("X-Mirror-Cache"))

	second := get(t, e, "/api/v3/widgets/7")
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, string(OutcomeHit), second.Header().Get("X-Mirror-Cache"),
		"the prefixed spelling shares the bare one's rows, so the second read is a hit")
	assert.EqualValues(t, 1, calls.Load(), "a hit costs no upstream call")
	assert.Equal(t, first.Body.String(), second.Body.String(),
		"hit and miss answer the same bytes whichever spelling asked")
}

// A rewrite is a spelling, not a door. The rewritten path meets every gate the
// bare path does, so a route needing a credential still needs it.
func TestRewrite_GrantsNoAccessOfItsOwn(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"someone"}`))
	}), func(s *Spec) {
		s.Rewrites = append(s.Rewrites, &Rewrite{From: "/api/v3"})
		s.Resources = append(s.Resources, credentialResource())
		s.Routes = append(s.Routes, &Route{Method: "GET", Path: "/user", Resource: "identity"})
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v3/user", nil))
	assert.Equal(t, string(PassNoIdentity), rec.Header().Get("X-Mirror-Passthrough-Reason"),
		"a credential-keyed route refuses the prefixed spelling exactly as it refuses the bare one")
}

func TestRewrite_MatchesOnlyAtASegmentBoundary(t *testing.T) {
	e := &Engine{spec: &Spec{Rewrites: []*Rewrite{
		{From: "/api/v3"},
		{From: "/api/graphql", To: "/graphql"},
	}}}

	for _, tc := range []struct {
		in, want string
		changed  bool
	}{
		{"/api/v3/repos/o/r", "/repos/o/r", true},
		{"/api/v3", "/", true},
		{"/api/graphql", "/graphql", true},
		{"/api/v3thing", "/api/v3thing", false},
		{"/repos/o/r", "/repos/o/r", false},
		{"/graphql", "/graphql", false},
	} {
		got, changed := e.rewritePath(tc.in)
		assert.Equal(t, tc.want, got, tc.in)
		assert.Equal(t, tc.changed, changed, tc.in)
	}
}

func TestValidateRewrite(t *testing.T) {
	for name, tc := range map[string]struct {
		rw    *Rewrite
		valid bool
	}{
		"a bare prefix strips":         {&Rewrite{From: "/api/v3"}, true},
		"a prefix may map to another":  {&Rewrite{From: "/api/graphql", To: "/graphql"}, true},
		"from must be absolute":        {&Rewrite{From: "api/v3"}, false},
		"from must not be empty":       {&Rewrite{From: ""}, false},
		"a trailing slash is refused":  {&Rewrite{From: "/api/v3/"}, false},
		"to must be absolute or empty": {&Rewrite{From: "/api/v3", To: "v3"}, false},
		"a rule that changes nothing":  {&Rewrite{From: "/api/v3", To: "/api/v3"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.valid {
				require.NoError(t, tc.rw.validate())
				return
			}
			require.Error(t, tc.rw.validate())
		})
	}
}
