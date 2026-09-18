package mirror

import (
	"context"
	"net/http"
)

// recordDelivered makes a row the delivery stated in full as fresh as a fetch
// would have: a read after the delivery serves it without asking the upstream.
//
// Only a delivery that writes every field of the resource states the whole
// answer. A partial one leaves freshness alone, so a row it created still
// fetches the fields it never carried.
func (i *Ingest) recordDelivered(ctx context.Context, res *Resource, ev *Event, key map[string]string) error {
	if !statesEveryField(res, ev) {
		return nil
	}
	now := i.now()
	return i.store.RecordFetched(ctx, Freshness{
		Kind:      res.Name,
		Key:       keyString(res, key),
		FetchedAt: now,
		ChangedAt: now,
		ExpiresAt: now.Add(res.TTL),
		State:     string(StateFresh),
		Status:    http.StatusOK,
	})
}

// statesEveryField reports whether an event's sets write every field of res.
func statesEveryField(res *Resource, ev *Event) bool {
	if len(res.Fields) == 0 {
		return false
	}
	for _, f := range res.Fields {
		found := false
		for _, st := range ev.Sets {
			if st.Field == f.Name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
