package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"
)

// fetchPlan is what a detached fetch needs to know about the request that
type fetchPlan struct {
	route *Route
	res   *Resource
	key   map[string]string
	query map[string]string
	path  string
}

type planKey struct{}

func withPlan(ctx context.Context, p *fetchPlan) context.Context {
	return context.WithValue(ctx, planKey{}, p)
}

func planFrom(ctx context.Context) *fetchPlan {
	p, _ := ctx.Value(planKey{}).(*fetchPlan)
	return p
}

// fetch refreshes key from the upstream and absorbs the answer.
//
// It stores state, never bytes. What a consumer receives is rebuilt from that
// state, so the shape of an answer is the spec's and cannot drift with whatever
// the upstream added this week.
func (e *Engine) fetch(ctx context.Context, kind, key, etag string) (FetchResult, error) {
	plan := planFrom(ctx)
	if plan == nil {
		return FetchResult{}, fmt.Errorf("fetch %s/%s: no plan on the context", kind, key)
	}
	answer, err := e.up.Call(ctx, plan.route.Method, plan.upstreamPath(), e.vars, forwardFrom(ctx))
	if err != nil {
		return FetchResult{}, err
	}
	if answer.Status == 304 {
		return FetchResult{ETag: etag}, nil
	}
	if !storable(e.up, plan.route, answer) {
		// The route models the request but not this answer. Storing it anyway
		// would be a guess written down as a fact.
		return FetchResult{}, fmt.Errorf("upstream %s answered %d, which route %s does not absorb",
			plan.path, answer.Status, plan.route.Path)
	}
	if answer.Overflow {
		return FetchResult{}, fmt.Errorf("upstream %s answered more than the %d byte cap", plan.path, maxBodyBytes)
	}

	result := FetchResult{ETag: answer.Header.Get("ETag"), Changed: true, Status: answer.Status}
	if answer.Status >= 400 {
		// The route declared this refusal worth keeping. It is recorded on the
		return result, nil
	}

	doc, err := decodeJSON(answer.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("upstream %s: %w", plan.path, err)
	}
	if err := e.absorbAnswer(ctx, plan, doc); err != nil {
		return FetchResult{}, err
	}
	return result, nil
}

// upstreamPath rebuilds the path this plan asks the upstream for, carrying only
// the query the route models. A parameter the route never modelled cannot reach
func (p *fetchPlan) upstreamPath() string {
	q := url.Values{}
	for _, decl := range p.route.Query {
		if v := p.query[decl.Name]; v != "" {
			q.Set(decl.Name, v)
		}
	}
	if len(q) == 0 {
		return p.path
	}
	return p.path + "?" + q.Encode()
}

// storable reports whether an answer is the route said to keep.
//
// A 2xx is always kept. A 4xx is kept only when the route names it, which is
func storable(up *Upstreamer, rt *Route, a *Answer) bool {
	if up.Transient(a) {
		return false
	}
	if a.Status >= 200 && a.Status < 300 {
		return true
	}
	for _, code := range rt.Absorb {
		if code == a.Status {
			return true
		}
	}
	return false
}

// absorbAnswer projects an upstream document onto the resource's columns.
func (e *Engine) absorbAnswer(ctx context.Context, plan *fetchPlan, doc any) error {
	now := time.Now()
	if plan.res.Store == StoreDocument {
		body, err := marshalJSON(trim(plan.res, doc))
		if err != nil {
			return err
		}
		row := Row{"document": string(body)}
		for k, v := range plan.key {
			row[k] = v
		}
		return e.store.Put(ctx, plan.res, row, now)
	}

	items, ok := doc.([]any)
	if !plan.route.List {
		return e.absorbOne(ctx, plan, doc, now)
	}
	if !ok {
		return fmt.Errorf("route %s is a list, but the upstream answered a single document", plan.route.Path)
	}
	rows := make([]Row, 0, len(items))
	for _, item := range items {
		row, err := absorb(plan.res, trim(plan.res, item))
		if err != nil {
			return err
		}
		// A list route's own key components pin the rows it owns, so an item
		// that does not carry them is still filed where the list will find it.
		for k, v := range plan.key {
			if _, ok := row[k]; !ok {
				row[k] = v
			}
		}
		if err := requireKeys(plan.res, row); err != nil {
			return fmt.Errorf("route %s: %w", plan.route.Path, err)
		}
		rows = append(rows, row)
	}
	if plan.route.Complete {
		// The route says this answer is the whole set, so an item missing from
		return e.store.ReplaceMany(ctx, plan.res, plan.key, rows, now)
	}
	return e.store.PutMany(ctx, plan.res, rows, now)
}

func (e *Engine) absorbOne(ctx context.Context, plan *fetchPlan, doc any, now time.Time) error {
	row, err := absorb(plan.res, trim(plan.res, doc))
	if err != nil {
		return err
	}
	// The request's own key wins over anything read out of the document: the
	// caller asked for this key, and the row must be findable under it.
	for k, v := range plan.key {
		row[k] = v
	}
	if err := requireKeys(plan.res, row); err != nil {
		return fmt.Errorf("route %s: %w", plan.route.Path, err)
	}
	return e.store.Put(ctx, plan.res, row, now)
}

// requireKeys refuses a row whose identity is incomplete. A row filed under a
// missing key is a row nothing ever finds again, and a webhook naming the real
// key would write a beside it.
func requireKeys(res *Resource, row Row) error {
	for _, k := range res.Keys {
		v, ok := row[k.Name]
		if !ok || v == nil || v == "" {
			return fmt.Errorf("resource %q: nothing supplied key %q", res.Name, k.Name)
		}
	}
	return nil
}

// A resource key becomes text in exactly place, keyString in reveal.go.

// fingerprint reduces a credential to a stable, non-reversible identifier. The
func fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
