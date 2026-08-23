package main

import (
	"fmt"
	"strings"
	"time"
)

// buildEvents reads the webhook ingest declaration.
//
// It lives beside the other builders rather than inside load.go so the whole
// ingest feature reads in one place.
func buildEvents(n *node) (*Events, error) {
	if err := checkAttrs(n, "path", "signature-header", "type-header", "reorder-window"); err != nil {
		return nil, err
	}
	e := &Events{
		Path:            n.Attr("path"),
		SignatureHeader: n.Attr("signature-header"),
		TypeHeader:      n.Attr("type-header"),
	}
	if w := n.Attr("reorder-window"); w != "" {
		d, err := parseWindow(w)
		if err != nil {
			return nil, err
		}
		e.ReorderWindow = d
	}
	for _, child := range n.Children() {
		switch child.Name {
		case "secret":
			s, err := compileContent(child)
			if err != nil {
				return nil, fmt.Errorf("<secret>: %w", err)
			}
			e.Secret = strings.TrimSpace(s)
		case "event":
			ev, err := buildEvent(child)
			if err != nil {
				return nil, err
			}
			e.List = append(e.List, ev)
		default:
			return nil, fmt.Errorf("<events>: unexpected child element <%s>", child.Name)
		}
	}
	return e, nil
}

// maxReorderWindow caps how long a delivery may be held.
//
// Every delivery waits the window, and a provider's own delivery timeout is
// single-digit seconds. Past that cap a latency knob becomes lost deliveries,
// so a fat-fingered value fails at load rather than wedging ingest.
const maxReorderWindow = 5 * time.Second

func parseWindow(v string) (time.Duration, error) {
	if v == "0" || v == "0s" {
		// Zero is a real choice: dispatch on arrival and leave ordering
		// entirely to the watermark.
		return 0, nil
	}
	d, err := parseDuration("<events reorder-window>", v)
	if err != nil {
		return 0, err
	}
	if d > maxReorderWindow {
		return 0, fmt.Errorf("<events reorder-window>: %s is longer than the %s cap -- a provider gives up on a delivery it cannot hand over in seconds", v, maxReorderWindow)
	}
	return d, nil
}

func buildEvent(n *node) (*Event, error) {
	if err := checkAttrs(n, "type", "resource", "clock", "unordered", "absorb-when-superseded"); err != nil {
		return nil, err
	}
	ev := &Event{
		Type:                 n.Attr("type"),
		Resource:             n.Attr("resource"),
		Clock:                n.Attr("clock"),
		Unordered:            n.Attr("unordered") == "true",
		AbsorbWhenSuperseded: n.Attr("absorb-when-superseded") == "true",
	}
	for _, child := range n.Children() {
		switch child.Name {
		case "subject":
			s, err := compileContent(child)
			if err != nil {
				return nil, fmt.Errorf("event %q subject: %w", ev.Type, err)
			}
			ev.Subject = strings.TrimSpace(s)
		case "apply":
			sets, err := buildSets(child, ev.Type)
			if err != nil {
				return nil, err
			}
			ev.Sets = append(ev.Sets, sets...)
		case "invalidate":
			if err := checkAttrs(child, "reason"); err != nil {
				return nil, err
			}
			ev.Invalidate = &Invalidate{Reason: child.Attr("reason")}
		default:
			return nil, fmt.Errorf("<event type=%q>: unexpected child element <%s>", ev.Type, child.Name)
		}
	}
	return ev, nil
}

func buildSets(n *node, eventType string) ([]Set, error) {
	if err := checkAttrs(n); err != nil {
		return nil, err
	}
	var out []Set
	for _, child := range n.Children() {
		if child.Name != "set" {
			return nil, fmt.Errorf("event %q: <apply> holds <set> elements, not <%s>", eventType, child.Name)
		}
		if err := checkAttrs(child, "field", "expr", "null"); err != nil {
			return nil, err
		}
		s := Set{
			Field:     child.Attr("field"),
			Expr:      child.Attr("expr"),
			AllowNull: child.Attr("null") == "allow",
		}
		if v := child.Attr("null"); v != "" && v != "allow" {
			return nil, fmt.Errorf("event %q set %q: null=%q -- the only value is \"allow\"", eventType, s.Field, v)
		}
		if s.Expr == "" {
			from, err := textOf(child)
			if err != nil {
				return nil, err
			}
			s.From = strings.TrimSpace(from)
		}
		out = append(out, s)
	}
	return out, nil
}
