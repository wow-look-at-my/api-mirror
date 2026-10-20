package mirror

import (
	"context"
)

// operatorPrincipal owns the subscriptions made through the dashboard.
const operatorPrincipal = "operator"

// visibleTo is the fan-out's gate: may this principal hear about this delivery.
// A notification reaches a principal only when it could have read the row
// without a probe.
func (e *Engine) visibleTo(ctx context.Context, principal string, d *Delivery) bool {
	res, ok := e.store.Resource(d.Event.Resource)
	if !ok {
		return false
	}
	key, _, err := ingestKey(res, d.Event, d.Payload)
	if err != nil {
		logf("notify: gate %s: %v", d.ID, err)
		return false
	}
	return e.reveal.Visible(ctx, principal, res, key)
}
