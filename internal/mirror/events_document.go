package mirror

import (
	"context"
	"fmt"
)

// A delivery that carries a whole document writes it.
//
// A document resource stores the upstream's answer whole. Some deliveries
// carry exactly that answer -- a workflow run event holds the run, a check
// run event holds the check run. So the delivery IS the new answer.
// <document> names where in the payload it sits.

// validateEventDocument refuses a <document> the engine cannot write.
func validateEventDocument(ev *Event, res *Resource) error {
	if ev.Document == "" {
		return nil
	}
	if res.Store != StoreDocument {
		return fmt.Errorf("event %q: <document> writes a whole answer, and resource %q stores columns", ev.Type, res.Name)
	}
	if len(ev.Sets) > 0 || ev.Invalidate != nil {
		return fmt.Errorf("event %q: <document> already replaces the row, so <set> and <invalidate> beside it say nothing", ev.Type)
	}
	return nil
}

// putDocument writes the payload's object at ev.Document as the stored
// answer. A payload with no object there fails the delivery, loudly: a row
// left as it was would serve the state the delivery replaced.
func (i *Ingest) putDocument(ctx context.Context, res *Resource, ev *Event, key, sent map[string]string, payload any) error {
	doc, ok := lookupPath(payload, ev.Document).(map[string]any)
	if !ok {
		return fmt.Errorf("event %q: the payload carries no object at %s", ev.Type, ev.Document)
	}
	body, err := marshalJSON(trim(res, doc))
	if err != nil {
		return err
	}
	row := Row{"document": string(body)}
	for name, v := range key {
		row[name] = v
	}
	for column, v := range sent {
		row[column] = v
	}
	return i.store.Put(ctx, res, row, i.now())
}
