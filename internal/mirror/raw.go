package mirror

import "context"

// rawDoc is a stored body that is served byte for byte, never re-marshalled.
type rawDoc string

// rawRow files an upstream body under the plan's key.
func (p *fetchPlan) rawRow(body []byte) Row {
	row := Row{"document": string(body)}
	for k, v := range p.key {
		row[k] = v
	}
	return row
}

// media asks the upstream for a raw resource in its route's media type
// instead of the spec's default Accept.
func (p *fetchPlan) media(ctx context.Context) context.Context {
	if p.res.Store != StoreRaw {
		return ctx
	}
	return context.WithValue(ctx, mediaKey{}, p.route.Accept[0])
}

type mediaKey struct{}

// mediaFrom is the Accept a raw fetch asks for, or "".
func mediaFrom(ctx context.Context) string {
	m, _ := ctx.Value(mediaKey{}).(string)
	return m
}
