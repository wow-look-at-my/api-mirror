package mirror

import (
	"testing"

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

func TestValidateRejectsAnEventDeclaredTwice(t *testing.T) {
	s := eventSpec()
	s.Events.List = append(s.Events.List, s.Events.List[0])
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice",
		"two handlers for one type means only one of them ever runs, silently")
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
