package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Load reads a mirror spec from disk.
//
// Parsing and validation are separate steps on purpose: a builder checks the
// SHAPE of the file (an unknown element, a missing attribute), and validate
// checks what the shape MEANS (a route pointing at nothing, a resource nobody
// gated). A message from the first tells an author about their typo; a message
// from the second tells them about their design.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open spec %q: %w", path, err)
	}
	spec, err := ParseSpec(raw)
	if err != nil {
		return nil, fmt.Errorf("parse spec %q: %w", path, err)
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("validate spec %q: %w", path, err)
	}
	return spec, nil
}

// ParseSpec builds a Spec from XML source.
func ParseSpec(raw []byte) (*Spec, error) {
	root, err := parseDOM(raw)
	if err != nil {
		return nil, err
	}
	if root.Name() != "mirror" {
		return nil, fmt.Errorf("root element is <%s>, expected <mirror>", root.Name())
	}
	return buildSpec(root)
}

func buildSpec(n *node) (*Spec, error) {
	if err := checkAttrs(n, "name", "schema"); err != nil {
		return nil, err
	}
	spec := &Spec{Name: n.Attr("name")}
	for _, child := range n.Children() {
		switch child.Name() {
		case "description":
			// Prose for a human reading the file. It is not used at run time.
		case "vars":
			vars, err := buildVars(child)
			if err != nil {
				return nil, err
			}
			spec.Vars = vars
		case "upstream":
			up, err := buildUpstream(child)
			if err != nil {
				return nil, err
			}
			spec.Upstream = up
		case "resource":
			r, err := buildResource(child)
			if err != nil {
				return nil, err
			}
			spec.Resources = append(spec.Resources, r)
		case "route":
			rt, err := buildRoute(child)
			if err != nil {
				return nil, err
			}
			spec.Routes = append(spec.Routes, rt)
		case "events":
			ev, err := buildEvents(child)
			if err != nil {
				return nil, err
			}
			spec.Events = ev
		default:
			return nil, fmt.Errorf("<mirror>: unexpected child element <%s>", child.Name())
		}
	}
	return spec, nil
}

func buildVars(n *node) ([]Var, error) {
	if err := checkAttrs(n); err != nil {
		return nil, err
	}
	var out []Var
	for _, child := range n.Children() {
		if child.Name() != "var" {
			return nil, fmt.Errorf("<vars>: unexpected child element <%s>", child.Name())
		}
		if err := checkAttrs(child, "name"); err != nil {
			return nil, err
		}
		name := child.Attr("name")
		if name == "" {
			return nil, fmt.Errorf("<var>: name is required")
		}
		value, err := compileContent(child)
		if err != nil {
			return nil, fmt.Errorf("<var name=%q>: %w", name, err)
		}
		out = append(out, Var{Name: name, Value: value})
	}
	return out, nil
}

func buildUpstream(n *node) (Upstream, error) {
	if err := checkAttrs(n, "base"); err != nil {
		return Upstream{}, err
	}
	up := Upstream{Base: n.Attr("base")}
	for _, child := range n.Children() {
		switch child.Name() {
		case "header":
			h, err := buildHeader(child)
			if err != nil {
				return Upstream{}, err
			}
			up.Headers = append(up.Headers, h)
		case "forward":
			if err := checkAttrs(child, "name"); err != nil {
				return Upstream{}, err
			}
			name := child.Attr("name")
			if name == "" {
				return Upstream{}, fmt.Errorf("<forward>: name is required")
			}
			up.Forward = append(up.Forward, name)
		default:
			return Upstream{}, fmt.Errorf("<upstream>: unexpected child element <%s>", child.Name())
		}
	}
	return up, nil
}

func buildHeader(n *node) (Header, error) {
	if err := checkAttrs(n, "name"); err != nil {
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
	return Header{Name: name, Value: value}, nil
}

func buildResource(n *node) (*Resource, error) {
	if err := checkAttrs(n, "name", "ttl", "store"); err != nil {
		return nil, err
	}
	r := &Resource{Name: n.Attr("name"), Store: StoreColumns}
	if s := n.Attr("store"); s != "" {
		r.Store = StoreMode(s)
	}
	if ttl := n.Attr("ttl"); ttl != "" {
		d, err := parseDuration(fmt.Sprintf("resource %q ttl", r.Name), ttl)
		if err != nil {
			return nil, err
		}
		r.TTL = d
	}
	for _, child := range n.Children() {
		if err := addResourceChild(r, child); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func addResourceChild(r *Resource, child *node) error {
	switch child.Name() {
	case "key":
		if err := checkAttrs(child, "name", "from", "fold"); err != nil {
			return err
		}
		r.Keys = append(r.Keys, Key{
			Name: child.Attr("name"),
			From: child.Attr("from"),
			Fold: child.Attr("fold") == "true",
		})
	case "field":
		f, err := buildField(child)
		if err != nil {
			return fmt.Errorf("resource %q: %w", r.Name, err)
		}
		r.Fields = append(r.Fields, f)
	case "drop":
		if err := checkAttrs(child, "key"); err != nil {
			return err
		}
		r.Drop = append(r.Drop, child.Attr("key"))
	case "keep":
		if err := checkAttrs(child, "name", "reason"); err != nil {
			return err
		}
		r.Keep = append(r.Keep, Keep{Name: child.Attr("name"), Reason: child.Attr("reason")})
	case "reveal":
		rv, err := buildReveal(child)
		if err != nil {
			return fmt.Errorf("resource %q: %w", r.Name, err)
		}
		r.Reveal = rv
	default:
		return fmt.Errorf("<resource name=%q>: unexpected child element <%s>", r.Name, child.Name())
	}
	return nil
}

func buildField(n *node) (Field, error) {
	if err := checkAttrs(n, "name", "type", "expr"); err != nil {
		return Field{}, err
	}
	f := Field{Name: n.Attr("name"), Type: FieldType(n.Attr("type")), Expr: n.Attr("expr")}
	if f.Type == "" {
		f.Type = FieldText
	}
	if f.Expr == "" {
		from, err := textOf(n)
		if err != nil {
			return Field{}, err
		}
		f.From = strings.TrimSpace(from)
	}
	return f, nil
}

func buildReveal(n *node) (*Reveal, error) {
	if err := checkAttrs(n); err != nil {
		return nil, err
	}
	rv := &Reveal{}
	for _, child := range n.Children() {
		switch child.Name() {
		case "public":
			p, err := compileContent(child)
			if err != nil {
				return nil, err
			}
			rv.Public = strings.TrimSpace(p)
		case "probe":
			if err := checkAttrs(child, "method", "path"); err != nil {
				return nil, err
			}
			method := child.Attr("method")
			if method == "" {
				method = "GET"
			}
			rv.Probe = &Probe{Method: method, Path: child.Attr("path")}
		case "grant":
			d, err := ttlAttr(child, "<grant>")
			if err != nil {
				return nil, err
			}
			rv.GrantTTL = d
		case "deny":
			d, err := ttlAttr(child, "<deny>")
			if err != nil {
				return nil, err
			}
			rv.DenyTTL = d
		default:
			return nil, fmt.Errorf("<reveal>: unexpected child element <%s>", child.Name())
		}
	}
	return rv, nil
}

func ttlAttr(n *node, what string) (time.Duration, error) {
	if err := checkAttrs(n, "ttl"); err != nil {
		return 0, err
	}
	ttl := n.Attr("ttl")
	if ttl == "" {
		return 0, fmt.Errorf("%s: ttl is required", what)
	}
	return parseDuration(what+" ttl", ttl)
}

func buildRoute(n *node) (*Route, error) {
	if err := checkAttrs(n, "method", "path", "resource", "ttl", "list", "complete"); err != nil {
		return nil, err
	}
	rt := &Route{
		Method:   strings.ToUpper(n.Attr("method")),
		Path:     n.Attr("path"),
		Resource: n.Attr("resource"),
		List:     n.Attr("list") == "true",
		Complete: n.Attr("complete") == "true",
	}
	if rt.Method == "" {
		rt.Method = "GET"
	}
	if ttl := n.Attr("ttl"); ttl != "" {
		d, err := parseDuration(fmt.Sprintf("route %s ttl", rt.Path), ttl)
		if err != nil {
			return nil, err
		}
		rt.TTL = d
	}
	for _, child := range n.Children() {
		if err := addRouteChild(rt, child); err != nil {
			return nil, err
		}
	}
	return rt, nil
}

func addRouteChild(rt *Route, child *node) error {
	switch child.Name() {
	case "param":
		p, err := buildQueryParam(child)
		if err != nil {
			return fmt.Errorf("route %s: %w", rt.Path, err)
		}
		rt.Query = append(rt.Query, p)
	case "accept":
		if err := checkAttrs(child); err != nil {
			return err
		}
		media, err := textOf(child)
		if err != nil {
			return err
		}
		rt.Accept = append(rt.Accept, strings.TrimSpace(media))
	case "absorb":
		if err := checkAttrs(child, "status"); err != nil {
			return err
		}
		code, err := strconv.Atoi(child.Attr("status"))
		if err != nil {
			return fmt.Errorf("route %s: <absorb status=%q> is not a number", rt.Path, child.Attr("status"))
		}
		rt.Absorb = append(rt.Absorb, code)
	case "map":
		if err := checkAttrs(child, "param", "key"); err != nil {
			return err
		}
		if rt.Params == nil {
			rt.Params = make(map[string]string)
		}
		rt.Params[child.Attr("param")] = child.Attr("key")
	default:
		return fmt.Errorf("<route path=%q>: unexpected child element <%s>", rt.Path, child.Name())
	}
	return nil
}

func buildQueryParam(n *node) (QueryParam, error) {
	if err := checkAttrs(n, "name", "type", "default", "min", "max", "key"); err != nil {
		return QueryParam{}, err
	}
	p := QueryParam{
		Name:    n.Attr("name"),
		Type:    FieldType(n.Attr("type")),
		Default: n.Attr("default"),
		Key:     n.Attr("key") == "true",
	}
	if p.Type == "" {
		p.Type = FieldText
	}
	for _, spec := range []struct {
		attr string
		dst  *int
	}{{"min", &p.Min}, {"max", &p.Max}} {
		if v := n.Attr(spec.attr); v != "" {
			num, err := strconv.Atoi(v)
			if err != nil {
				return QueryParam{}, fmt.Errorf("<param name=%q>: %s=%q is not a number", p.Name, spec.attr, v)
			}
			*spec.dst = num
		}
	}
	return p, nil
}
