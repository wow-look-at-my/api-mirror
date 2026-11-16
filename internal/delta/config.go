// config.go holds TruthName.

package delta

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
	"gopkg.in/yaml.v3"
)

// TruthName is the name the served target and every report give the truth.
const TruthName = "truth"

// Config is one delta run: where the truth is, which mirrors to hold against
// it, and which differences are expected.
type Config struct {
	Listen  string   `yaml:"listen"`
	Truth   Target   `yaml:"truth"`
	Mirrors []Target `yaml:"mirrors"`
	// Serve names the target whose answer the client receives.
	Serve string `yaml:"serve"`
	// Compare selects the requests that fan out.
	Compare []Match `yaml:"compare"`
	// Headers are the response headers compared. Most headers differ on every request, so the list is opt-in.
	Headers []string `yaml:"headers"`
	// Annotate names mirror response headers copied into each record.
	Annotate []string `yaml:"annotate"`
	Report   string   `yaml:"report"`
	Recent   int      `yaml:"recent"`
	Timeout  Duration `yaml:"timeout"`
	Ignore   []*Rule  `yaml:"ignore"`
}

// Target is one API base URL. Headers replace the client's value on every
// request to that target, and ${VAR} in a value reads the environment.
type Target struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`

	base string
}

// Match selects requests by method and by a regular expression on the path.
// An empty field matches anything.
type Match struct {
	Method string `yaml:"method"`
	Route  string `yaml:"route"`

	route *regexp.Regexp
}

func (m *Match) compile() error {
	m.Method = strings.ToUpper(m.Method)
	if m.Route == "" {
		return nil
	}
	re, err := regexp.Compile(m.Route)
	if err != nil {
		return fmt.Errorf("route %q: %w", m.Route, err)
	}
	m.route = re
	return nil
}

func (m *Match) matches(method, path string) bool {
	return (m.Method == "" || m.Method == method) && (m.route == nil || m.route.MatchString(path))
}

// Rule ignores the differences it selects. Path selects JSON differences by
// location, Header selects one header, and Kinds narrows either.
type Rule struct {
	Match  `yaml:",inline"`
	Path   string `yaml:"path"`
	Header string `yaml:"header"`
	Kinds  []Kind `yaml:"kinds"`
	Mirror string `yaml:"mirror"`
	Reason string `yaml:"reason"`

	pattern *Pattern
}

// Describe is a one-line name for the rule in a report.
func (r *Rule) Describe() string {
	var parts []string
	if r.Method != "" || r.Route != "" {
		parts = append(parts, strings.TrimSpace(r.Method+" "+r.Route))
	}
	if r.Mirror != "" {
		parts = append(parts, "mirror="+r.Mirror)
	}
	if r.Path != "" {
		parts = append(parts, r.Path)
	}
	if r.Header != "" {
		parts = append(parts, "header "+r.Header)
	}
	if len(r.Kinds) > 0 {
		ks := make([]string, len(r.Kinds))
		for i, k := range r.Kinds {
			ks[i] = string(k)
		}
		parts = append(parts, "("+strings.Join(ks, ",")+")")
	}
	return strings.Join(parts, " ")
}

func (r *Rule) covers(method, path, mirror string, d Diff) bool {
	if !r.matches(method, path) || r.Mirror != "" && r.Mirror != mirror {
		return false
	}
	if len(r.Kinds) > 0 && !slices.Contains(r.Kinds, d.Kind) {
		return false
	}
	switch {
	case r.pattern != nil:
		return d.Kind.isJSON() && r.pattern.Covers(d.loc)
	case r.Header != "":
		return d.Kind == KindHeader && strings.EqualFold(r.Header, d.Path)
	}
	return true
}

// Duration reads a Go duration string such as 30s.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// LoadConfig reads a YAML config. An unknown key is an error, so a typo in
// an ignore rule cannot silently ignore nothing.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate fills defaults and compiles every pattern. It reports every
// problem at once.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8090"
	}
	if c.Recent == 0 {
		c.Recent = 500
	}
	if c.Recent < 0 {
		errs = append(errs, fmt.Errorf("recent: %d is negative", c.Recent))
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(60 * time.Second)
	}
	if c.Timeout < 0 {
		errs = append(errs, fmt.Errorf("timeout: %s is negative", time.Duration(c.Timeout)))
	}
	if c.Headers == nil {
		c.Headers = []string{"Content-Type", "Link", "Location"}
	}
	if c.Annotate == nil {
		c.Annotate = []string{"X-Mirror-Cache", "X-Mirror-Passthrough-Reason"}
	}
	if c.Compare == nil {
		c.Compare = []Match{{Method: http.MethodGet}, {Method: http.MethodHead}}
	}
	if c.Serve == "" {
		c.Serve = TruthName
	}

	if c.Truth.Name == "" {
		c.Truth.Name = TruthName
	}
	if c.Truth.Name != TruthName {
		errs = append(errs, fmt.Errorf("truth: the name is always %q", TruthName))
	}
	if c.Truth.URL == "" {
		errs = append(errs, errors.New("truth: url is required"))
	}
	if len(c.Mirrors) == 0 {
		errs = append(errs, errors.New("mirrors: at least one mirror is required"))
	}
	names := set.Of(TruthName)
	targets := append([]*Target{&c.Truth}, pointers(c.Mirrors)...)
	for i, t := range targets {
		if i > 0 {
			if t.Name == "" {
				errs = append(errs, fmt.Errorf("mirrors[%d]: name is required", i-1))
			} else if !names.Add(t.Name) {
				errs = append(errs, fmt.Errorf("mirrors[%d]: name %q is used twice", i-1, t.Name))
			}
		}
		if err := t.resolve(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
		}
	}
	if !names.Contains(c.Serve) {
		errs = append(errs, fmt.Errorf("serve: %q names no target", c.Serve))
	}
	for i := range c.Compare {
		if err := c.Compare[i].compile(); err != nil {
			errs = append(errs, fmt.Errorf("compare[%d]: %w", i, err))
		}
	}
	for i, r := range c.Ignore {
		if err := r.validate(names); err != nil {
			errs = append(errs, fmt.Errorf("ignore[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func pointers(ts []Target) []*Target {
	out := make([]*Target, len(ts))
	for i := range ts {
		out[i] = &ts[i]
	}
	return out
}

func (t *Target) resolve() error {
	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("url %q: need an absolute http or https URL", t.URL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("url %q: a base URL carries no query or fragment", t.URL)
	}
	t.base = strings.TrimRight(t.URL, "/")
	var missing []string
	for k, v := range t.Headers {
		t.Headers[k] = os.Expand(v, func(name string) string {
			val, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("headers: environment variable %s is not set", strings.Join(slices.Compact(missing), ", "))
	}
	return nil
}

func (r *Rule) validate(targets set.Set[string]) error {
	if err := r.compile(); err != nil {
		return err
	}
	for _, k := range r.Kinds {
		if !slices.Contains(allKinds, k) {
			return fmt.Errorf("kind %q: want one of %v", k, allKinds)
		}
	}
	if r.Mirror != "" && (r.Mirror == TruthName || !targets.Contains(r.Mirror)) {
		return fmt.Errorf("mirror %q names no mirror", r.Mirror)
	}
	if r.Path != "" && r.Header != "" {
		return errors.New("path and header select different differences; write two rules")
	}
	if r.Path != "" {
		p, err := ParsePattern(r.Path)
		if err != nil {
			return err
		}
		r.pattern = p
		for _, k := range r.Kinds {
			if !k.isJSON() {
				return fmt.Errorf("path selects JSON differences, and kind %q has no JSON path", k)
			}
		}
	}
	if r.Header != "" {
		for _, k := range r.Kinds {
			if k != KindHeader {
				return fmt.Errorf("header selects header differences, and kind %q is not one", k)
			}
		}
	}
	if r.Path == "" && r.Header == "" && len(r.Kinds) == 0 {
		return errors.New("a rule must name a path, a header or a kind; one that names none ignores everything")
	}
	return nil
}

// target finds a target by name.
func (c *Config) target(name string) *Target {
	if name == TruthName {
		return &c.Truth
	}
	for i := range c.Mirrors {
		if c.Mirrors[i].Name == name {
			return &c.Mirrors[i]
		}
	}
	return nil
}

func (c *Config) compares(method, path string) bool {
	for i := range c.Compare {
		if c.Compare[i].matches(method, path) {
			return true
		}
	}
	return false
}

func (c *Config) ruleFor(method, path, mirror string, d Diff) int {
	for i, r := range c.Ignore {
		if r.covers(method, path, mirror, d) {
			return i
		}
	}
	return -1
}
