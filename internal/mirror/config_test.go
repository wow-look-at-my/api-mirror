package mirror

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goodSpec is the smallest spec the engine will serve: one keyed resource with
// columns and a reveal rule, one route that can key it.
func goodSpec() *Spec {
	return &Spec{
		Name:     "example",
		Upstream: Upstream{Base: "https://api.example.com"},
		Resources: []*Resource{{
			Name:  "repo",
			Store: StoreColumns,
			TTL:   time.Hour,
			Keys:  []Key{{Name: "owner"}, {Name: "name"}},
			Fields: []Field{
				{Name: "visibility", Type: FieldText, From: "visibility"},
			},
			Reveal: &Reveal{Public: `{{ eq .row.visibility "public" }}`},
		}},
		Routes: []*Route{{
			Method:   "GET",
			Path:     "/repos/{owner}/{name}",
			Resource: "repo",
		}},
	}
}

func TestValidateAcceptsMinimalSpec(t *testing.T) {
	require.NoError(t, goodSpec().validate())
}

func TestValidateRejectsResourceWithoutReveal(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Reveal = nil
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "who may read it")
}

func TestValidateRejectsRevealThatProvesNothing(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Reveal = &Reveal{}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proves nothing")
}

func TestValidateRejectsProbeWithoutGrantTTL(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Reveal = &Reveal{
		Probe:   &Probe{Method: "GET", Path: "/repos/{{ .key.owner }}/{{ .key.name }}"},
		DenyTTL: 5 * time.Minute,
	}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never expires is not a proof")
}

func TestValidateRejectsKeylessResource(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Keys = nil
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no identity")
}

func TestValidateRejectsRouteThatCannotKeyItsResource(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Path = "/repos/{owner}"
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `nothing supplies "name"`)
}

func TestValidateAcceptsRouteParamRenaming(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Path = "/repos/{owner}/{repo}"
	s.Routes[0].Params = map[string]string{"repo": "name"}
	require.NoError(t, s.validate())
}

func TestValidateRejectsWriteRoute(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Method = "POST"
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only reads and credential-gated mints are cached")
}

func TestValidateRejectsUnknownResourceOnRoute(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Resource = "nope"
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestValidateRejectsColumnsResourceWithNoFields(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Fields = nil
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty answer")
}

func TestValidateRejectsDocumentResourceWithFields(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Store = StoreDocument
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would never be read")
}

// eventSpec adds a webhook declaration to the minimal spec.
func eventSpec() *Spec {
	s := goodSpec()
	s.Events = &Events{
		Path:            "/webhook",
		Secret:          "{{ .env.WEBHOOK_SECRET }}",
		SignatureHeader: "X-Hub-Signature-256",
		TypeHeader:      "X-Event",
		ReorderWindow:   2 * time.Second,
		List: []*Event{{
			Type:     "repository",
			Resource: "repo",
			Subject:  "{{ .payload.repository.full_name }}",
			Clock:    "repository.updated_at",
			Sets: []Set{
				{Field: "visibility", From: "repository.visibility"},
			},
		}},
	}
	return s
}

func TestValidateAcceptsEvents(t *testing.T) {
	require.NoError(t, eventSpec().validate())
}

func TestValidateRejectsEventWithoutClock(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Clock = ""
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overwrite newer truth silently")
}

func TestValidateAcceptsExplicitlyUnorderedEvent(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Clock = ""
	s.Events.List[0].Unordered = true
	require.NoError(t, s.validate())
}

func TestValidateRejectsEventWithoutSubject(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Subject = ""
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "order against each other")
}

func TestValidateRejectsInvalidateWithoutReason(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Sets = nil
	s.Events.List[0].Invalidate = &Invalidate{}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a reason")
}

func TestValidateAcceptsInvalidateWithReason(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Sets = nil
	s.Events.List[0].Invalidate = &Invalidate{Reason: "a rename does not state the new full name"}
	require.NoError(t, s.validate())
}

func TestValidateRejectsSetOfUndeclaredField(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Sets = []Set{{Field: "nope", From: "x"}}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not declare")
}

func TestValidateRejectsUnsignedEvents(t *testing.T) {
	s := eventSpec()
	s.Events.Secret = ""
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "anyone's delivery")
}

func TestPathParams(t *testing.T) {
	assert.Equal(t, []string{"owner", "repo"}, pathParams("/repos/{owner}/{repo}/pulls"))
	assert.Empty(t, pathParams("/rate_limit"))
}

func TestParseDuration(t *testing.T) {
	d, err := parseDuration("ttl", "90s")
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)

	_, err = parseDuration("ttl", "0s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be positive")

	_, err = parseDuration("ttl", "soon")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a duration")
}

func TestValidateRejectsSpecWithoutAName(t *testing.T) {
	s := goodSpec()
	s.Name = "   "
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a name")
}

func TestValidateRejectsUpstreamWithoutABase(t *testing.T) {
	s := goodSpec()
	s.Upstream.Base = ""
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a base URL")
}

func TestValidateRejectsAResourceDeclaredTwice(t *testing.T) {
	s := goodSpec()
	s.Resources = append(s.Resources, s.Resources[0])
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice",
		"two resources of one name would derive two tables of one name")
}

func TestValidateRejectsAnUnnamedResource(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Name = " "
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<resource> needs a name")
}

func TestValidateRejectsAnUnnamedKeyAndADuplicateOne(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Keys = []Key{{Name: ""}}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<key> needs a name")

	s = goodSpec()
	s.Resources[0].Keys = []Key{{Name: "owner"}, {Name: "owner"}}
	err = s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice",
		"one column cannot be two key components; the primary key would name it twice")
}

func TestValidateRejectsAnUnknownStoreMode(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Store = StoreMode("blob")
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown store mode")
}

func TestValidateRejectsFieldMistakes(t *testing.T) {
	cases := []struct {
		name  string
		field Field
		want  string
	}{
		{name: "no name", field: Field{Type: FieldText, From: "a"}, want: "<field> needs a name"},
		{
			name:  "a name a key already took",
			field: Field{Name: "owner", Type: FieldText, From: "a"},
			want:  "declared twice",
		},
		{name: "an unknown type", field: Field{Name: "x", Type: "colour", From: "a"}, want: "unknown type"},
		{
			name:  "neither a path nor an expr",
			field: Field{Name: "x", Type: FieldText},
			want:  "not both and not neither",
		},
		{
			name:  "both a path and an expr",
			field: Field{Name: "x", Type: FieldText, From: "a", Expr: "{{ .a }}"},
			want:  "not both and not neither",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSpec()
			s.Resources[0].Fields = append(s.Resources[0].Fields, tc.field)
			err := s.validate()
			require.Error(t, err, "a field with %s cannot be turned into a column that reads back", tc.name)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A keep is a claim that some consumer needs a field the drop rule would take.
// Without the consumer named, the hole in the rule has no owner and no expiry.
func TestValidateRejectsAKeepThatExplainsNothing(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Drop = []string{"*url"}
	s.Resources[0].Keep = []Keep{{Name: "html_url"}}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a reason")

	s.Resources[0].Keep = []Keep{{Name: "", Reason: "because"}}
	err = s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<keep> needs a name")
}

func TestValidateRejectsAKeepWithNoDropToRescueFrom(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Keep = []Keep{{Name: "html_url", Reason: "the viewer links back to the page"}}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a <drop> it does not have",
		"a keep with no drop reads as protection and provides none")
}

func TestValidateRejectsAProbeWithoutAPathOrADenyTTL(t *testing.T) {
	s := goodSpec()
	s.Resources[0].Reveal = &Reveal{Probe: &Probe{Method: "GET"}, GrantTTL: time.Hour, DenyTTL: time.Minute}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<probe> needs a path")

	s.Resources[0].Reveal = &Reveal{
		Probe:    &Probe{Method: "GET", Path: "/repos/{{ .key.owner }}"},
		GrantTTL: time.Hour,
	}
	err = s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<deny ttl=>",
		"with no deny TTL every refused caller re-asks the upstream on every request")
}

func TestValidateRejectsARelativeRoutePath(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Path = "repos/{owner}/{name}"
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
}

func TestValidateAcceptsAListRouteThatSuppliesNoKey(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Path = "/repos"
	s.Routes[0].List = true
	require.NoError(t, s.validate(),
		"a list route's parent keys select rows; it does not have to name every key component")
}

func TestValidateAcceptsAKeyedQueryParamAsAKeySource(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Path = "/repos/{owner}"
	s.Routes[0].Query = []QueryParam{{Name: "name", Type: FieldText, Key: true}}
	require.NoError(t, s.validate(), "a keyed query parameter supplies a key component just as a path segment does")
}

func TestValidateRejectsQueryMistakes(t *testing.T) {
	cases := []struct {
		name  string
		query []QueryParam
		want  string
	}{
		{name: "no name", query: []QueryParam{{Type: FieldText, Key: true}}, want: "<param> needs a name"},
		{
			name:  "declared twice",
			query: []QueryParam{{Name: "page", Type: FieldInt, Key: true}, {Name: "page", Type: FieldInt, Key: true}},
			want:  "declared twice",
		},
		{name: "no type", query: []QueryParam{{Name: "page", Key: true}}, want: "needs a type"},
		{
			name:  "a type a query value cannot carry",
			query: []QueryParam{{Name: "page", Type: FieldJSON, Key: true}},
			want:  "text, int or bool",
		},
		{
			name:  "a max below its min",
			query: []QueryParam{{Name: "page", Type: FieldInt, Key: true, Min: 10, Max: 1}},
			want:  "is below min",
		},
		{
			name:  "unkeyed with no default",
			query: []QueryParam{{Name: "page", Type: FieldInt}},
			want:  "share one cached answer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSpec()
			s.Routes[0].Query = tc.query
			err := s.validate()
			require.Error(t, err, "a param that is %s describes a cache key nobody can reason about", tc.name)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateRejectsAnAcceptThatIsNotAMediaType(t *testing.T) {
	s := goodSpec()
	s.Routes[0].Accept = []string{"json"}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a media type")
}

// A transient failure stored is an outage remembered long after it ended, so the
// spec is not allowed to declare one authoritative.
func TestValidateRejectsAbsorbingATransientStatus(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		s := goodSpec()
		s.Routes[0].Absorb = []int{code}
		err := s.validate()
		require.Errorf(t, err, "absorbing %d would cache an outage", code)
		assert.Contains(t, err.Error(), "refusing to absorb")
	}

	s := goodSpec()
	s.Routes[0].Absorb = []int{700}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not an HTTP status")

	s = goodSpec()
	s.Routes[0].Absorb = []int{404, 410}
	require.NoError(t, s.validate(), "a 4xx is the upstream stating a fact, and a route may name it")
}
