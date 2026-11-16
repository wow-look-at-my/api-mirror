// load_exchange.go holds buildCredential.

package mirror

import (
	"fmt"
	"github.com/wow-look-at-my/go-containers/set"
	"net/http"
	"strings"
)

// buildCredential reads a <credential>: a token the engine obtains by an
// exchange, for the calls that have no caller.
func buildCredential(n *node) (*Credential, error) {
	if err := checkAttrs(n, "name", "requires", "for", "margin"); err != nil {
		return nil, err
	}
	c := &Credential{Name: n.Attr("name"), Requires: n.Attr("requires"), For: n.Attr("for")}
	what := fmt.Sprintf("<credential name=%q>", c.Name)
	if v := n.Attr("margin"); v != "" {
		d, err := parseDuration(what+" margin", v)
		if err != nil {
			return nil, err
		}
		c.Margin = d
	}
	exchanges := 0
	for _, child := range n.Children() {
		switch child.Name() {
		case "header":
			h, err := buildHeader(child)
			if err != nil {
				return nil, err
			}
			c.Headers = append(c.Headers, h)
		case "lookup":
			if c.Lookup != nil {
				return nil, fmt.Errorf("%s: <lookup> declared twice", what)
			}
			l, err := buildCredentialLookup(what, child)
			if err != nil {
				return nil, err
			}
			c.Lookup = l
		case "exchange":
			x, err := buildCredentialExchange(what, child)
			if err != nil {
				return nil, err
			}
			c.Exchange = x
			exchanges++
		default:
			return nil, fmt.Errorf("%s: unexpected child element <%s>", what, child.Name())
		}
	}
	if exchanges != 1 {
		return nil, fmt.Errorf("%s needs exactly one <exchange>, found %d", what, exchanges)
	}
	return c, nil
}

func buildCredentialLookup(what string, n *node) (*CredentialLookup, error) {
	if err := checkAttrs(n, "method", "path", "field", "ttl"); err != nil {
		return nil, err
	}
	if len(n.Children()) > 0 {
		return nil, fmt.Errorf("%s: <lookup> takes no child elements", what)
	}
	l := &CredentialLookup{Method: strings.ToUpper(n.Attr("method")), Path: n.Attr("path"), Field: n.Attr("field")}
	if l.Method == "" {
		l.Method = http.MethodGet
	}
	if v := n.Attr("ttl"); v != "" {
		d, err := parseDuration(what+" <lookup> ttl", v)
		if err != nil {
			return nil, err
		}
		l.TTL = d
	}
	return l, nil
}

func buildCredentialExchange(what string, n *node) (CredentialExchange, error) {
	if err := checkAttrs(n, "method", "path", "token", "expires", "ttl"); err != nil {
		return CredentialExchange{}, err
	}
	if len(n.Children()) > 0 {
		return CredentialExchange{}, fmt.Errorf("%s: <exchange> takes no child elements", what)
	}
	x := CredentialExchange{
		Method:  strings.ToUpper(n.Attr("method")),
		Path:    n.Attr("path"),
		Token:   n.Attr("token"),
		Expires: n.Attr("expires"),
	}
	if x.Method == "" {
		x.Method = http.MethodPost
	}
	if v := n.Attr("ttl"); v != "" {
		d, err := parseDuration(what+" <exchange> ttl", v)
		if err != nil {
			return CredentialExchange{}, err
		}
		x.TTL = d
	}
	return x, nil
}

// buildUpstreamHeader reads an <upstream><header>, which alone may name a
// credential.
func buildUpstreamHeader(n *node) (Header, error) {
	if err := checkAttrs(n, "name", "credential"); err != nil {
		return Header{}, err
	}
	name := n.Attr("name")
	if name == "" {
		return Header{}, fmt.Errorf("<header>: name is required")
	}
	value, err := compileContent(n)
	if err != nil {
		return Header{}, fmt.Errorf("<header name=%q>: %w", name, err)
	}
	return Header{Name: name, Value: value, Credential: n.Attr("credential")}, nil
}

// validateCredentials checks that each credential can issue a token and
// that some upstream header sends it.
func (s *Spec) validateCredentials() error {
	byName := map[string]*Credential{}
	for _, c := range s.Credentials {
		if err := c.validate(); err != nil {
			return err
		}
		if _, dup := byName[c.Name]; dup {
			return fmt.Errorf("<credential name=%q> declared twice", c.Name)
		}
		byName[c.Name] = c
	}
	used := set.New[string]()
	for _, h := range s.Upstream.Headers {
		if h.Credential == "" {
			continue
		}
		if byName[h.Credential] == nil {
			return fmt.Errorf("<upstream><header name=%q> names credential %q, which no <credential> declares", h.Name, h.Credential)
		}
		used.Add(h.Credential)
	}
	for _, c := range s.Credentials {
		if !used.Contains(c.Name) {
			// A token nothing sends is an exchange nobody can see is idle.
			return fmt.Errorf("<credential name=%q>: no <upstream><header credential=%q> sends it", c.Name, c.Name)
		}
	}
	return nil
}

func (c *Credential) validate() error {
	what := fmt.Sprintf("<credential name=%q>", c.Name)
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("<credential> needs a name")
	}
	if c.Margin < 0 {
		return fmt.Errorf("%s: margin must not be negative", what)
	}
	x := c.Exchange
	if !strings.HasPrefix(x.Path, "/") {
		return fmt.Errorf("%s: <exchange> needs a path= starting with /", what)
	}
	if x.Token == "" {
		return fmt.Errorf("%s: <exchange> needs token=, the answer's path to the token", what)
	}
	if x.Expires == "" && x.TTL <= 0 {
		// Without either, a token would be reused after the upstream stopped honouring it.
		return fmt.Errorf("%s: <exchange> needs expires= or ttl=, so the engine knows when a token stops working", what)
	}
	if strings.Contains(x.Path, ".lookup") && c.Lookup == nil {
		return fmt.Errorf("%s: the <exchange> path reads .lookup, but no <lookup> is declared", what)
	}
	if l := c.Lookup; l != nil {
		if !strings.HasPrefix(l.Path, "/") || l.Field == "" {
			return fmt.Errorf("%s: <lookup> needs a path= starting with / and a field=", what)
		}
		if strings.Contains(l.Path, ".for") && c.For == "" {
			return fmt.Errorf("%s: the <lookup> path reads .for, but the credential declares no for=", what)
		}
	}
	if strings.Contains(x.Path, ".for") && c.For == "" {
		return fmt.Errorf("%s: the <exchange> path reads .for, but the credential declares no for=", what)
	}
	return nil
}
