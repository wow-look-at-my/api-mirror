// spread.go holds Spread.

package mirror

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Spread carries each item of a fetched answer into another resource.
type Spread struct {
	// Resource receives the items.
	Resource string
	// Items is the path to the array of items. Empty means the answer is the array.
	Items string
	// Absorb stores each item into Resource through that resource's own field.
	Absorb bool
	// Grant records a grant on each item for the caller that fetched it.
	Grant bool
	// Keys name where an item carries each key of Resource. Empty uses the resource's own from= paths.
	Keys []Set
}

// grantSourceSpread names a grant a listing earned.
const grantSourceSpread = "list_sync"

func buildSpread(n *node) (*Spread, error) {
	if err := checkAttrs(n, "resource", "items", "absorb", "grant"); err != nil {
		return nil, err
	}
	sp := &Spread{
		Resource: n.Attr("resource"),
		Items:    n.Attr("items"),
		Absorb:   n.Attr("absorb") != "false",
		Grant:    n.Attr("grant") != "false",
	}
	for _, child := range n.Children() {
		if child.Name() != "key" {
			return nil, fmt.Errorf("<spread>: unexpected child element <%s>", child.Name())
		}
		k, err := buildPathOrExpr(child, "field")
		if err != nil {
			return nil, err
		}
		sp.Keys = append(sp.Keys, k)
	}
	return sp, nil
}

func (sp *Spread) validate(route string, resources map[string]*Resource) error {
	res, ok := resources[sp.Resource]
	if !ok {
		return fmt.Errorf("route %s: <spread resource=%q> names no declared resource", route, sp.Resource)
	}
	if !sp.Absorb && !sp.Grant {
		return fmt.Errorf("route %s: <spread> that neither absorbs nor grants does nothing", route)
	}
	if sp.Absorb && res.Store == StoreDocument {
		return fmt.Errorf("route %s: <spread> cannot absorb items into %q, which stores a whole document", route, sp.Resource)
	}
	if sp.Grant && (res.Reveal == nil || res.Reveal.Credential || res.Reveal.GrantTTL <= 0) {
		return fmt.Errorf("route %s: <spread> grants on %q, which declares no <grant ttl>", route, sp.Resource)
	}
	for _, k := range sp.Keys {
		if !slices.ContainsFunc(res.Keys, func(rk Key) bool { return rk.Name == k.Field }) {
			return fmt.Errorf("route %s: <spread><key field=%q> is not a key of %q", route, k.Field, sp.Resource)
		}
	}
	if len(sp.Keys) > 0 {
		for _, rk := range res.Keys {
			if !slices.ContainsFunc(sp.Keys, func(k Set) bool { return k.Field == rk.Name }) {
				return fmt.Errorf("route %s: <spread> names some keys of %q, so it must name %q too", route, sp.Resource, rk.Name)
			}
		}
	}
	return nil
}

// spread applies a route's <spread> declarations to a fetched answer. The
// grant goes to the principal on the fetch's lane, the caller whose
// credential the upstream answered.
func (e *Engine) spread(ctx context.Context, plan *fetchPlan, doc any) error {
	if len(plan.route.Spreads) == 0 {
		return nil
	}
	principal := laneFrom(ctx).principal
	now := time.Now()
	for _, sp := range plan.route.Spreads {
		res, ok := e.store.Resource(sp.Resource)
		if !ok {
			return fmt.Errorf("spread into unknown resource %q", sp.Resource)
		}
		list := doc
		if sp.Items != "" {
			list = lookupPath(doc, sp.Items)
		}
		items, ok := list.([]any)
		if !ok {
			if list == nil {
				continue
			}
			return fmt.Errorf("route %s: <spread items=%q> is not an array", plan.route.Path, sp.Items)
		}
		skipped := 0
		for _, item := range items {
			key, ok := spreadKey(res, sp, item)
			if !ok {
				skipped++
				continue
			}
			if sp.Absorb {
				row, err := absorb(res, trim(res, item))
				if err != nil {
					return err
				}
				for k, v := range key {
					row[k] = v
				}
				if err := e.store.Put(ctx, res, row, now); err != nil {
					return err
				}
				// The row is as fresh as a fetch of it would be. A read of the item is a
				// hit rather than a second call for the same fact.
				if err := e.store.RecordFetched(ctx, Freshness{
					Kind: res.Name, Key: keyString(res, key), FetchedAt: now, ChangedAt: now,
					ExpiresAt: now.Add(e.ttlFor(res.Name)), State: string(StateFresh), Status: http.StatusOK,
				}); err != nil {
					return err
				}
			}
			if sp.Grant && principal != "" {
				if err := e.store.RecordGrant(ctx, Grant{
					Principal: principal, Resource: res.Name, Key: keyString(res, key),
					Source: grantSourceSpread, ExpiresAt: now.Add(res.Reveal.GrantTTL),
				}); err != nil {
					return err
				}
			}
		}
		if skipped > 0 {
			logf("spread %s -> %s: %d of %d items name no complete key and were skipped",
				plan.route.Path, sp.Resource, skipped, len(items))
		}
	}
	return nil
}

// spreadKey reads an item's key for the target resource.
func spreadKey(res *Resource, sp *Spread, item any) (map[string]string, bool) {
	key := make(map[string]string, len(res.Keys))
	for _, rk := range res.Keys {
		path := rk.From
		expr := ""
		for _, k := range sp.Keys {
			if k.Field == rk.Name {
				path, expr = k.From, k.Expr
			}
		}
		var v string
		if expr != "" {
			s, err := renderString(expr, map[string]any{"item": item})
			if err != nil {
				return nil, false
			}
			v = s
		} else {
			v = scalarString(lookupPath(item, path))
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, false
		}
		key[rk.Name] = foldKey(rk, v)
	}
	return key, true
}
