// load_ops.go holds buildPathOrExpr.

package mirror

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Builders for the operational half of a spec: the dashboard, the browser
// policy, the background jobs and the subscriber fan-out.
//
// They check SHAPE only -- an unknown attribute, a missing name. What a
// declaration MEANS is validate.go's job. A rule that is well-formed and
// wrong still fails at load rather than at in the morning.

func buildDashboard(n *node) (Dashboard, error) {
	if err := checkAttrs(n, "path", "title"); err != nil {
		return Dashboard{}, err
	}
	d := Dashboard{Path: n.Attr("path"), Title: n.Attr("title")}
	for _, child := range n.Children() {
		switch child.Name() {
		case "token":
			t, err := compileContent(child)
			if err != nil {
				return Dashboard{}, fmt.Errorf("<dashboard><token>: %w", err)
			}
			d.Token = t
		case "sign-in":
			attrs := []string{"client-id", "client-secret", "authorize", "exchange", "user", "login", "admins", "secret", "base-url"}
			if err := checkAttrs(child, attrs...); err != nil {
				return Dashboard{}, err
			}
			for _, required := range attrs[:6] {
				if child.Attr(required) == "" {
					return Dashboard{}, fmt.Errorf("<dashboard><sign-in> needs %s", required)
				}
			}
			d.SignIn = &SignIn{
				ClientID:     child.Attr("client-id"),
				ClientSecret: child.Attr("client-secret"),
				Authorize:    child.Attr("authorize"),
				Exchange:     child.Attr("exchange"),
				User:         child.Attr("user"),
				Login:        child.Attr("login"),
				Admins:       child.Attr("admins"),
				Secret:       child.Attr("secret"),
				BaseURL:      child.Attr("base-url"),
			}
		case "login":
			l, err := buildLogin(child)
			if err != nil {
				return Dashboard{}, err
			}
			d.Login = l
		default:
			return Dashboard{}, fmt.Errorf("<dashboard>: unexpected child element <%s>", child.Name())
		}
	}
	return d, nil
}

func buildCORS(n *node) (*CORS, error) {
	if err := checkAttrs(n, "max-age"); err != nil {
		return nil, err
	}
	c := &CORS{}
	if v := n.Attr("max-age"); v != "" {
		d, err := parseDuration("<cors> max-age", v)
		if err != nil {
			return nil, err
		}
		c.MaxAge = d
	}
	for _, child := range n.Children() {
		text, err := textOf(child)
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		switch child.Name() {
		case "origin":
			if err := checkAttrs(child); err != nil {
				return nil, err
			}
			// A rendered origin may be a comma-separated list, as an environment variable usually carries one.
			rendered, err := loadTimeValue("<cors><origin>", text)
			if err != nil {
				return nil, err
			}
			for _, o := range strings.Split(rendered, ",") {
				if o = strings.TrimSpace(o); o != "" {
					c.Origins = append(c.Origins, o)
				}
			}
		case "expose":
			if err := checkAttrs(child); err != nil {
				return nil, err
			}
			c.Expose = append(c.Expose, text)
		default:
			return nil, fmt.Errorf("<cors>: unexpected child element <%s>", child.Name())
		}
	}
	return c, nil
}

func buildNotify(n *node) (*Notify, error) {
	if err := checkAttrs(n, "path", "db", "import", "signature-header", "timeout", "retries", "disable-after",
		"scope-field", "event-header", "delivery-header"); err != nil {
		return nil, err
	}
	no := &Notify{
		Path:            n.Attr("path"),
		DB:              n.Attr("db"),
		Import:          n.Attr("import"),
		SignatureHeader: n.Attr("signature-header"),
		ScopeField:      n.Attr("scope-field"),
		EventHeader:     n.Attr("event-header"),
		DeliveryHeader:  n.Attr("delivery-header"),
	}
	if v := n.Attr("timeout"); v != "" {
		d, err := parseDuration("<notify> timeout", v)
		if err != nil {
			return nil, err
		}
		no.Timeout = d
	}
	for _, spec := range []struct {
		attr string
		dst  *int
	}{{"retries", &no.Retries}, {"disable-after", &no.DisableAfter}} {
		if v := n.Attr(spec.attr); v != "" {
			num, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("<notify>: %s=%q is not a number", spec.attr, v)
			}
			*spec.dst = num
		}
	}
	for _, child := range n.Children() {
		switch child.Name() {
		case "scope":
			if err := checkAttrs(child); err != nil {
				return nil, err
			}
			s, err := compileContent(child)
			if err != nil {
				return nil, fmt.Errorf("<notify><scope>: %w", err)
			}
			no.Scope = strings.TrimSpace(s)
		case "reveal":
			if err := checkAttrs(child, "resource"); err != nil {
				return nil, err
			}
			rv := &NotifyReveal{Resource: child.Attr("resource")}
			for _, k := range child.Children() {
				if k.Name() != "key" {
					return nil, fmt.Errorf("<notify><reveal>: unexpected child element <%s>", k.Name())
				}
				s, err := buildPathOrExpr(k, "field")
				if err != nil {
					return nil, err
				}
				rv.Keys = append(rv.Keys, s)
			}
			no.Reveal = rv
		case "field":
			s, err := buildPathOrExpr(child, "name")
			if err != nil {
				return nil, err
			}
			no.Fields = append(no.Fields, s)
		default:
			return nil, fmt.Errorf("<notify>: unexpected child element <%s>", child.Name())
		}
	}
	return no, nil
}

// buildPathOrExpr reads an element that names a value either as a payload
// path in its text or as template source in expr=.
func buildPathOrExpr(n *node, nameAttr string) (Set, error) {
	if err := checkAttrs(n, nameAttr, "expr"); err != nil {
		return Set{}, err
	}
	s := Set{Field: n.Attr(nameAttr), Expr: n.Attr("expr")}
	if s.Expr == "" {
		from, err := textOf(n)
		if err != nil {
			return Set{}, err
		}
		s.From = strings.TrimSpace(from)
	}
	return s, nil
}

func buildRefresh(n *node) (*Refresh, error) {
	if err := checkAttrs(n, "interval"); err != nil {
		return nil, err
	}
	r := &Refresh{}
	if v := strings.TrimSpace(n.Attr("interval")); v != "" {
		// The attribute may be template source, so resolve it before judging it.
		resolved, err := loadTimeValue("<refresh> interval", v)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(resolved) == "0" {
			// Zero is an operator turning the sweep off, the same way it turns <replay> off.
			return r, nil
		}
		d, err := parseDuration("<refresh> interval", v)
		if err != nil {
			return nil, err
		}
		r.Interval = d
	} else {
		return nil, fmt.Errorf(`<refresh> needs an interval, or interval="0" to turn the sweep off`)
	}
	for _, child := range n.Children() {
		if child.Name() != "kind" {
			return nil, fmt.Errorf("<refresh>: unexpected child element <%s>", child.Name())
		}
		if err := checkAttrs(child); err != nil {
			return nil, err
		}
		text, err := textOf(child)
		if err != nil {
			return nil, err
		}
		r.Kinds = append(r.Kinds, strings.TrimSpace(text))
	}
	return r, nil
}

func buildReplay(n *node) (*Replay, error) {
	if err := checkAttrs(n, "interval", "method", "lookback", "max", "requires"); err != nil {
		return nil, err
	}
	rp := &Replay{Method: strings.ToUpper(n.Attr("method")), Requires: strings.TrimSpace(n.Attr("requires"))}
	if v := n.Attr("max"); v != "" {
		num, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("<replay>: max=%q is not a number", v)
		}
		rp.Max = num
	}
	if v := n.Attr("interval"); v != "" {
		d, off, err := parseDurationOrOff("<replay> interval", v)
		if err != nil {
			return nil, err
		}
		rp.Interval, rp.Off = d, off
	}
	for attr, dst := range map[string]*time.Duration{"lookback": &rp.Lookback} {
		v := n.Attr(attr)
		if v == "" {
			continue
		}
		d, err := parseDuration("<replay> "+attr, v)
		if err != nil {
			return nil, err
		}
		*dst = d
	}
	for _, child := range n.Children() {
		if child.Name() == "header" {
			h, err := buildHeader(child)
			if err != nil {
				return nil, fmt.Errorf("<replay>: %w", err)
			}
			rp.Headers = append(rp.Headers, h)
			continue
		}
		if err := checkAttrs(child); err != nil {
			return nil, err
		}
		if child.Name() == "redeliver" {
			// Template source over `.delivery`; every other child is a path.
			compiled, err := compileContent(child)
			if err != nil {
				return nil, fmt.Errorf("<replay><redeliver>: %w", err)
			}
			rp.Redeliver = strings.TrimSpace(compiled)
			continue
		}
		text, err := textOf(child)
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		switch child.Name() {
		case "list":
			rp.List = text
		case "id":
			rp.ID = text
		case "at":
			rp.At = text
		default:
			return nil, fmt.Errorf("<replay>: unexpected child element <%s>", child.Name())
		}
	}
	return rp, nil
}

func buildHealth(n *node) (*Health, error) {
	if err := checkAttrs(n, "live", "pre-update"); err != nil {
		return nil, err
	}
	return &Health{Live: n.Attr("live"), PreUpdate: n.Attr("pre-update")}, nil
}

func buildIdentity(n *node) (*Identity, error) {
	if err := checkAttrs(n, "header", "method", "path", "send", "scheme", "id", "name", "prefix", "ttl"); err != nil {
		return nil, err
	}
	id := &Identity{
		Header: n.Attr("header"),
		Method: strings.ToUpper(n.Attr("method")),
		Path:   n.Attr("path"),
		Send:   n.Attr("send"),
		Scheme: n.Attr("scheme"),
		ID:     n.Attr("id"),
		Name:   n.Attr("name"),
		Prefix: n.Attr("prefix"),
	}
	if v := n.Attr("ttl"); v != "" {
		d, err := parseDuration("<identity> ttl", v)
		if err != nil {
			return nil, err
		}
		id.TTL = d
	}
	for _, child := range n.Children() {
		if err := checkAttrs(child, "header", "as", "scheme", "path", "principal", "name"); err != nil {
			return nil, err
		}
		rule := &IdentityRule{
			Header:    child.Attr("header"),
			As:        child.Attr("as"),
			Scheme:    child.Attr("scheme"),
			Path:      child.Attr("path"),
			Principal: child.Attr("principal"),
			Name:      child.Attr("name"),
		}
		switch child.Name() {
		case "user":
			id.User = rule
		case "assertion":
			id.Assertion = rule
		default:
			return nil, fmt.Errorf("<identity>: unexpected child element <%s>", child.Name())
		}
	}
	if (id.User != nil || id.Assertion != nil) && id.TTL <= 0 {
		return nil, fmt.Errorf("<identity> needs a positive ttl: an unremembered verdict costs an upstream call on every request")
	}
	return id, nil
}

func buildWhoami(n *node) (*Whoami, error) {
	if err := checkAttrs(n, "method", "path", "id", "prefix", "ttl"); err != nil {
		return nil, err
	}
	for _, child := range n.Children() {
		return nil, fmt.Errorf("<whoami>: unexpected child element <%s>", child.Name())
	}
	w := &Whoami{
		Method: strings.ToUpper(n.Attr("method")),
		Path:   n.Attr("path"),
		ID:     n.Attr("id"),
		Prefix: n.Attr("prefix"),
	}
	if v := n.Attr("ttl"); v != "" {
		d, err := parseDuration("<whoami> ttl", v)
		if err != nil {
			return nil, err
		}
		w.TTL = d
	}
	return w, nil
}

func buildRateHeaders(n *node) (RateHeaders, error) {
	if err := checkAttrs(n, "limit", "remaining", "used", "reset", "resource", "refusal", "answer", "poll", "poll-field"); err != nil {
		return RateHeaders{}, err
	}
	if (n.Attr("poll") == "") != (n.Attr("poll-field") == "") {
		return RateHeaders{}, fmt.Errorf("<ratelimit> poll and poll-field come together")
	}
	return RateHeaders{
		Poll:      n.Attr("poll"),
		PollField: n.Attr("poll-field"),
		Limit:     n.Attr("limit"),
		Remaining: n.Attr("remaining"),
		Used:      n.Attr("used"),
		Reset:     n.Attr("reset"),
		Resource:  n.Attr("resource"),
		Refusal:   n.Attr("refusal"),
		Answer:    n.Attr("answer"),
	}, nil
}
