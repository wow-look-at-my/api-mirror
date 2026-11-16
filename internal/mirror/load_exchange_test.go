package mirror

import (
	_ "embed"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed exchange_spec.xml
var credentialSpec string

func TestLoadReadsACredential(t *testing.T) {
	spec, err := ParseSpec([]byte(credentialSpec))
	require.NoError(t, err)
	require.NoError(t, spec.validate())

	require.Len(t, spec.Credentials, 1)
	c := spec.Credentials[0]
	assert.Equal(t, "app", c.Name)
	assert.Equal(t, "env.APP_ID", c.Requires)
	assert.Equal(t, "{{ .key.owner }}", c.For)
	assert.Equal(t, 2*time.Minute, c.Margin)
	require.Len(t, c.Headers, 1)
	require.NotNil(t, c.Lookup)
	assert.Equal(t, "GET", c.Lookup.Method, "a lookup reads by default")
	assert.Equal(t, 30*time.Minute, c.Lookup.TTL)
	assert.Equal(t, "POST", c.Exchange.Method, "an exchange posts by default")
	assert.Equal(t, "token", c.Exchange.Token)
	assert.Equal(t, "expires_at", c.Exchange.Expires)

	require.Len(t, spec.Upstream.Headers, 2)
	assert.Empty(t, spec.Upstream.Headers[0].Credential)
	assert.Equal(t, "app", spec.Upstream.Headers[1].Credential)
}

func TestLoadRefusesABadCredential(t *testing.T) {
	cases := []struct{ name, src, want string }{{
		name: "a credential= on a replay header",
		src:  `<mirror name="m"><replay interval="5m"><header name="A" credential="app">x</header></replay></mirror>`,
		want: `unknown attribute "credential"`,
	}, {
		name: "no exchange",
		src:  `<mirror name="m"><credential name="app"><lookup path="/x" field="id"/></credential></mirror>`,
		want: "needs exactly one <exchange>",
	}, {
		name: "an unknown child",
		src:  `<mirror name="m"><credential name="app"><token/></credential></mirror>`,
		want: "unexpected child element <token>",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSpec([]byte(tc.src))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateRefusesACredentialThatCannotWork(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*Spec)
		want  string
	}{{
		name:  "a header naming an undeclared credential",
		tweak: func(s *Spec) { s.Upstream.Headers[1].Credential = "other" },
		want:  `names credential "other", which no <credential> declares`,
	}, {
		name:  "a credential no header sends",
		tweak: func(s *Spec) { s.Upstream.Headers[1].Credential = ""; s.Upstream.Headers[1].Value = "x" },
		want:  `no <upstream><header credential="app"> sends it`,
	}, {
		name:  "no token path",
		tweak: func(s *Spec) { s.Credentials[0].Exchange.Token = "" },
		want:  "needs token=",
	}, {
		name:  "no lifetime",
		tweak: func(s *Spec) { s.Credentials[0].Exchange.Expires = "" },
		want:  "needs expires= or ttl=",
	}, {
		name:  "a .lookup with no lookup",
		tweak: func(s *Spec) { s.Credentials[0].Lookup = nil },
		want:  "reads .lookup, but no <lookup> is declared",
	}, {
		name:  "a .for with no for=",
		tweak: func(s *Spec) { s.Credentials[0].For = "" },
		want:  "reads .for, but the credential declares no for=",
	}, {
		name:  "a relative exchange path",
		tweak: func(s *Spec) { s.Credentials[0].Exchange.Path = "tokens" },
		want:  "<exchange> needs a path= starting with /",
	}, {
		name:  "a duplicate",
		tweak: func(s *Spec) { s.Credentials = append(s.Credentials, s.Credentials[0]) },
		want:  "declared twice",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseSpec([]byte(credentialSpec))
			require.NoError(t, err)
			tc.tweak(spec)
			err = spec.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
