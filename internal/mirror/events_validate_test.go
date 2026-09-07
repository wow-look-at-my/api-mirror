package mirror

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What an events declaration MEANS, rather than what ingest does with.
// Each case names a delivery the engine could not handle honestly if the spec
// were accepted.

func TestValidateRejectsEventsWithoutTheirHeaders(t *testing.T) {
	cases := []struct {
		name string
		bend func(*Events)
		want string
	}{
		{name: "no path", bend: func(e *Events) { e.Path = "" }, want: "<events> needs a path"},
		{
			name: "no signature header",
			bend: func(e *Events) { e.SignatureHeader = "" },
			want: "needs a signature header",
		},
		{name: "no type header", bend: func(e *Events) { e.TypeHeader = "" }, want: "needs a type header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := eventSpec()
			tc.bend(s.Events)
			err := s.validate()
			require.Error(t, err, "ingest with %s cannot verify or classify a delivery", tc.name)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateRejectsEventMistakes(t *testing.T) {
	cases := []struct {
		name string
		bend func(*Event)
		want string
	}{
		{name: "no type", bend: func(e *Event) { e.Type = "" }, want: "<event> needs a type"},
		{
			name: "an undeclared resource",
			bend: func(e *Event) { e.Resource = "nope" },
			want: "which is not declared",
		},
		{
			name: "both a clock and unordered",
			bend: func(e *Event) { e.Unordered = true },
			want: "both a clock and unordered",
		},
		{
			name: "neither a set nor an invalidate",
			bend: func(e *Event) { e.Sets = nil },
			want: "does nothing",
		},
		{
			name: "a set and an invalidate together",
			bend: func(e *Event) { e.Invalidate = &Invalidate{Reason: "r"} },
			want: "the write already replaced the row",
		},
		{
			name: "a set with neither a path nor an expr",
			bend: func(e *Event) { e.Sets = []Set{{Field: "visibility"}} },
			want: "not both and not neither",
		},
		{
			name: "a set with both a path and an expr",
			bend: func(e *Event) { e.Sets = []Set{{Field: "visibility", From: "a", Expr: "{{ .a }}"}} },
			want: "not both and not neither",
		},
		{
			name: "a key that is not a key of the resource",
			bend: func(e *Event) { e.Keys = append(e.Keys, Set{Field: "visibility", From: "a"}) },
			want: "is not a key of resource",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := eventSpec()
			tc.bend(s.Events.List[0])
			err := s.validate()
			require.Error(t, err, "an event with %s writes something nobody can predict", tc.name)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateRefusesAnEventThatCannotAddressItsRow(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[0].Keys = nil

	err := spec.validate()
	require.Error(t, err, "left to run time this is one failed delivery per delivery, forever")
	assert.Contains(t, err.Error(), `cannot address key "owner"`)
}

func TestValidateRefusesAnInvalidateThatNamesNoKey(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[1].Keys = nil

	err := spec.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalidates every row")
}

func TestValidateRejectsAnEventDeclaredTwiceForOneResource(t *testing.T) {
	s := eventSpec()
	s.Events.List = append(s.Events.List, s.Events.List[0])
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice",
		"two handlers for one type and one resource means the second silently decides the row")
}

// A delivery moves several resources. A push states a branch tip AND makes the
// repo's file-derived answers wrong, so the type fans out per resource.
func TestValidateAcceptsATypeThatFansOutAcrossResources(t *testing.T) {
	s := eventSpec()
	s.Resources = append(s.Resources, &Resource{
		Name:   "readme",
		Store:  StoreDocument,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "owner"}, {Name: "name"}},
		Reveal: &Reveal{Public: `{{ true }}`},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "GET", Path: "/repos/{owner}/{name}/readme", Resource: "readme",
	})

	other := *s.Events.List[0]
	other.Resource = "readme"
	other.Sets = nil
	other.Invalidate = &Invalidate{Reason: "the payload names the changed files and never their content"}
	s.Events.List = append(s.Events.List, &other)

	require.NoError(t, s.validate())
}

func TestValidateAcceptsAnEventSettingAKeyComponent(t *testing.T) {
	s := eventSpec()
	s.Events.List[0].Sets = []Set{{Field: "owner", From: "repository.owner.login"}}
	require.NoError(t, s.validate(), "a key component is a declared column and an event may write it")
}

func TestValidateRejectsAnEventWritingFieldsIntoADocumentResource(t *testing.T) {
	s := eventSpec()
	s.Resources[0].Store = StoreDocument
	s.Resources[0].Fields = nil
	s.Events.List[0].Sets = []Set{{Field: "owner", From: "repository.owner.login"}}
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which stores a document",
		"a document resource has no columns, so a set would write into nothing")
}
