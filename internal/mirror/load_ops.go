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
// declaration MEANS is validate.go's job, so a rule that is well-formed and
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
			c.Origins = append(c.Origins, text)
		case "expose":
			if err := checkAttrs(child); err != nil {
				return nil, err
			}
			c.Expose = append(c.Expose, text)
		default:
			return nil, fmt.Errorf("<cors>: unexpected child element <%s>", child.Name())
		}
	}
	// Always exposed: no author should list the engine's own headers.
	c.Expose = append(c.Expose, defaultExposed...)
	return c, nil
}

func buildNotify(n *node) (*Notify, error) {
	if err := checkAttrs(n, "path", "db", "signature-header", "timeout", "retries", "disable-after"); err != nil {
		return nil, err
	}
	no := &Notify{
		Path:            n.Attr("path"),
		DB:              n.Attr("db"),
		SignatureHeader: n.Attr("signature-header"),
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
		return nil, fmt.Errorf("<notify>: unexpected child element <%s>", child.Name())
	}
	return no, nil
}

func buildRefresh(n *node) (*Refresh, error) {
	if err := checkAttrs(n, "interval"); err != nil {
		return nil, err
	}
	r := &Refresh{}
	if v := n.Attr("interval"); v != "" {
		d, err := parseDuration("<refresh> interval", v)
		if err != nil {
			return nil, err
		}
		r.Interval = d
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
	for attr, dst := range map[string]*time.Duration{"interval": &rp.Interval, "lookback": &rp.Lookback} {
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

func buildRateHeaders(n *node) (RateHeaders, error) {
	if err := checkAttrs(n, "limit", "remaining", "used", "reset", "resource"); err != nil {
		return RateHeaders{}, err
	}
	return RateHeaders{
		Limit:     n.Attr("limit"),
		Remaining: n.Attr("remaining"),
		Used:      n.Attr("used"),
		Reset:     n.Attr("reset"),
		Resource:  n.Attr("resource"),
	}, nil
}
