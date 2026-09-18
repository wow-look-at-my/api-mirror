package mirror

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// LiveRate is a single App installation's budget, asked for rather than
// observed. The passive meter only knows an identity that has made a call; a
// quiet installation near its limit is invisible to it.
type LiveRate struct {
	Account   string       `json:"account"`
	Resources []RateBudget `json:"resources"`
	Error     string       `json:"error,omitempty"`
}

// liveRates asks the declared budget path a single time per installation,
// with that installation's own token. The note says why the list is empty when it is.
func (e *Engine) liveRates(ctx context.Context) ([]LiveRate, string) {
	rule := e.spec.Upstream.Rate
	if rule.Poll == "" {
		return nil, "no <ratelimit poll> declared: showing observed budgets only"
	}
	if e.up.app == nil {
		return nil, "no App configured: live per-installation polling is unavailable; showing observed budgets only"
	}
	owners, err := e.up.app.owners(ctx)
	if err != nil {
		return nil, "listing installations failed: " + err.Error()
	}
	out := make([]LiveRate, 0, len(owners))
	for _, owner := range owners {
		live := LiveRate{Account: owner, Resources: []RateBudget{}}
		resources, err := e.pollRate(ctx, owner)
		if err != nil {
			live.Error = err.Error()
		} else {
			live.Resources = resources
		}
		out = append(out, live)
	}
	return out, ""
}

func (e *Engine) pollRate(ctx context.Context, owner string) ([]RateBudget, error) {
	rule := e.spec.Upstream.Rate
	app := e.up.app.rule
	ctx = withPlan(withLane(ctx, LaneAdmin, "", "rate-poll"), &fetchPlan{key: map[string]string{app.OwnerKey: owner}})
	answer, err := e.up.Call(ctx, http.MethodGet, rule.Poll, e.vars, nil, nil)
	if err != nil {
		return nil, err
	}
	if answer.Status != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", rule.Poll, answer.Status)
	}
	doc, err := decodeJSON(answer.Body)
	if err != nil {
		return nil, err
	}
	byResource, ok := lookupPath(doc, rule.PollField).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s carries no map at %q", rule.Poll, rule.PollField)
	}
	now := time.Now()
	out := make([]RateBudget, 0, len(byResource))
	for name, v := range byResource {
		num := func(field string) int {
			f, _ := strconv.ParseFloat(fmt.Sprint(lookupPath(v, field)), 64)
			return int(f)
		}
		b := RateBudget{
			Principal:  "app-installation:" + owner,
			Resource:   name,
			Limit:      num("limit"),
			Remaining:  num("remaining"),
			Used:       num("used"),
			ObservedAt: now,
		}
		if reset := num("reset"); reset > 0 {
			b.Reset = time.Unix(int64(reset), 0)
			b.Stale = b.Reset.Before(now)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out, nil
}
