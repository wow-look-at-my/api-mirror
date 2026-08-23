package main

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
	assert.Contains(t, err.Error(), "only reads are cached")
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
