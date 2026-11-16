// events_patch.go holds patchPages.

package mirror

import (
	"context"
	"fmt"
	"slices"
)

// A delivery that changes one item of a paginated document list patches it.
//
// A page-keyed list cannot be written whole from a payload that names one
// item. A payload that states the item's new value still corrects each stored
// page that lists the item. The patch element names how to find the item. Its
// sets name the fields the payload states.

// validateEventPatch refuses a <patch> the engine cannot apply.
func validateEventPatch(ev *Event, res *Resource, routes []*Route) error {
	p := ev.Patch
	if p == nil {
		return nil
	}
	if res.Store != StoreDocument || !slices.ContainsFunc(routes, func(rt *Route) bool { return rt.List && rt.Resource == res.Name }) {
		return fmt.Errorf("event %q: <patch> rewrites items in stored pages, and resource %q is not a document list -- give it store=\"document\" and a route with list=\"true\", or write it with <apply>", ev.Type, res.Name)
	}
	if len(ev.Sets) > 0 || ev.Document != "" || ev.Invalidate != nil {
		return fmt.Errorf("event %q: <patch> beside <apply>, <document> or <invalidate> leaves two writers for one row; keep one", ev.Type)
	}
	if ev.AbsorbWhenSuperseded {
		return fmt.Errorf("event %q: <patch> writes a value a later delivery can move, so it cannot be absorb-when-superseded", ev.Type)
	}
	if p.Match == "" {
		return fmt.Errorf("event %q: <patch> needs match=, the item path that finds the item the delivery is about", ev.Type)
	}
	if (p.From == "") == (p.Expr == "") {
		return fmt.Errorf("event %q: <patch match=%q> needs from= (a payload path) or expr= (a template) for the value to match, not both and not neither", ev.Type, p.Match)
	}
	if len(p.Sets) == 0 {
		return fmt.Errorf("event %q: <patch> sets nothing -- add <set field=\"item.path\">payload path</set> for each field the delivery states", ev.Type)
	}
	for _, st := range p.Sets {
		if st.Field == "" {
			return fmt.Errorf("event %q: a <patch> <set> needs field=, the item path it writes", ev.Type)
		}
		if (st.From == "") == (st.Expr == "") {
			return fmt.Errorf("event %q patch set %q: give it a payload path or an expr, not both and not neither", ev.Type, st.Field)
		}
	}
	return nil
}

// patchPages rewrites the matching item in every stored page under key.
//
// A patch leaves freshness alone: it corrects one item and restates no page,
// as a partial <apply> does. So a patched page keeps the TTL its fetch set.
//
// A page with no matching item is not touched. A stored refusal has no row,
// so it is never a page here. Any failure drops every page under key, because
// a page that may list the value must not keep serving it.
func (i *Ingest) patchPages(ctx context.Context, res *Resource, ev *Event, key map[string]string, payload any) (DeliveryDisposition, error) {
	changed, err := i.patchEach(ctx, res, ev.Patch, key, payload)
	if err == nil {
		if changed == 0 {
			return DeliveryIgnored, nil
		}
		return DeliveryApplied, nil
	}
	logf("event %q: patch %s under %s failed, dropping those pages instead: %v", ev.Type, res.Name, keyString(res, key), err)
	if _, err := i.store.Delete(ctx, res, key); err != nil {
		return DeliveryFailed, err
	}
	i.forgetFreshness(ctx, res, key, false)
	return DeliveryInvalidated, nil
}

// patchEach patches every stored page under key and reports how many changed.
func (i *Ingest) patchEach(ctx context.Context, res *Resource, p *Patch, key map[string]string, payload any) (int, error) {
	want, err := setValue(Set{From: p.From, Expr: p.Expr}, FieldText, payload)
	if err != nil {
		return 0, fmt.Errorf("match value: %w", err)
	}
	match := scalarString(want)
	if match == "" {
		return 0, fmt.Errorf("the payload states no value to match %s against", p.Match)
	}
	values := make([]any, len(p.Sets))
	for n, st := range p.Sets {
		v, err := setValue(st, FieldText, payload)
		if err != nil {
			return 0, fmt.Errorf("set %q: %w", st.Field, err)
		}
		if v == nil && !st.AllowNull {
			return 0, fmt.Errorf("set %q: the payload states no new value", st.Field)
		}
		values[n] = v
	}
	rows, err := i.store.List(ctx, res, key)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, row := range rows {
		body, ok := patchPage(row["document"], p, match, values)
		if body == "" && ok {
			continue
		}
		if !ok {
			return changed, fmt.Errorf("page %s is not a stored array", keyString(res, rowKey(res, row)))
		}
		row["document"] = body
		if err := i.store.Put(ctx, res, row, i.now()); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// patchPage writes values into each item whose match path equals match. It
// returns the new body, or "" when no item matched. ok is false for a page
// that is not a JSON array.
func patchPage(cell any, p *Patch, match string, values []any) (string, bool) {
	var raw string
	switch c := cell.(type) {
	case string:
		raw = c
	case []byte:
		raw = string(c)
	default:
		return "", false
	}
	doc, err := decodeJSON([]byte(raw))
	if err != nil {
		return "", false
	}
	items, ok := doc.([]any)
	if !ok {
		return "", false
	}
	hit := false
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok || scalarString(lookupPath(obj, p.Match)) != match {
			continue
		}
		for n, st := range p.Sets {
			setPath(obj, st.Field, values[n])
		}
		hit = true
	}
	if !hit {
		return "", true
	}
	body, err := marshalJSON(items)
	if err != nil {
		return "", false
	}
	return string(body), true
}
