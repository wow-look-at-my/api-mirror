package mirror

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Checking the cache against upstream truth.
//
// Every other view here reports what THIS PROCESS has seen: what it fetched,
// what was delivered to it, what it answered. None of that can show a fact the
// mirror never learned was wrong -- a delivery that never arrived leaves a row
// that is well-formed, recent-looking and stale, and no amount of watching the
// traffic reveals it. The only thing that does is asking the upstream again and
// comparing.

// CheckVerdict is what one key's comparison came to. The vocabulary is closed:
// a verdict with no name here is a row an operator cannot decide about.
type CheckVerdict string

const (
	// CheckAgrees means every stored column matched.
	CheckAgrees CheckVerdict = "agrees"
	// CheckDrifted means the stored row and the upstream disagree.
	CheckDrifted CheckVerdict = "drifted"
	// CheckRaced means the row changed while this key was being fetched, so a
	// difference may be the write that landed rather than drift.
	CheckRaced CheckVerdict = "raced"
	// CheckGone means the upstream no longer has what the row describes.
	CheckGone CheckVerdict = "gone"
	// CheckUnreachable means the upstream could not be asked. Never drift: a
	// 5xx says nothing at all about whether the stored answer is right.
	CheckUnreachable CheckVerdict = "unreachable"
	// CheckUnsupported means this key cannot be checked, and says why. Named
	// rather than skipped: a key quietly left out reads as a key that agreed.
	CheckUnsupported CheckVerdict = "unsupported"
)

// Difference is one column the two sides disagree about.
type Difference struct {
	Field    string `json:"field"`
	Stored   string `json:"stored"`
	Upstream string `json:"upstream"`
}

// KeyCheck is one key's verdict, as the page and the NDJSON stream carry it.
type KeyCheck struct {
	Kind     string       `json:"kind"`
	Key      string       `json:"key"`
	Verdict  CheckVerdict `json:"verdict"`
	Diffs    []Difference `json:"diffs,omitempty"`
	Detail   string       `json:"detail,omitempty"`
	Repaired bool         `json:"repaired,omitempty"`
}

// checkKeyTimeout bounds one key's fetch, so one wedged upstream response
// cannot hold a whole check open.
const checkKeyTimeout = 30 * time.Second

// Check re-asks the upstream about every stored key of one kind and reports
// where the cache and the upstream disagree.
//
// It uses the mirror's own credential, which is what makes it an operator tool
// rather than a consumer one: a caller's token would answer for that caller,
// and the question here is whether the SHARED row is right. repair writes the
// upstream's answer over a row that drifted.
func (e *Engine) Check(ctx context.Context, kind string, repair bool, emit func(KeyCheck)) error {
	keys, err := e.store.FreshnessByKind(ctx, kind)
	if err != nil {
		return err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	for _, f := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		emit(e.checkOne(ctx, kind, f, repair))
	}
	return nil
}

func (e *Engine) checkOne(ctx context.Context, kind string, f Freshness, repair bool) KeyCheck {
	out := KeyCheck{Kind: kind, Key: f.Key}

	plan, ok := e.planFor(kind, StaleKey{Key: f.Key})
	if !ok {
		out.Verdict = CheckUnsupported
		out.Detail = "this key cannot be turned back into a request: a credential-keyed resource, or a route that no longer covers it"
		return out
	}
	if plan.route.List {
		// A list key stands for a set, and the upstream's page boundaries are
		// not the mirror's. Comparing one page to one key would report drift
		// wherever pagination differs, which is worse than reporting nothing.
		out.Verdict = CheckUnsupported
		out.Detail = "a list route holds a set under one key; comparing it needs the whole set, not one page"
		return out
	}

	stored, err := e.store.Get(ctx, plan.res, plan.key)
	if err != nil {
		out.Verdict = CheckUnreachable
		out.Detail = "read the stored row: " + err.Error()
		return out
	}

	fetchCtx, cancel := context.WithTimeout(withLane(ctx, LaneRefresh, "", "check"), checkKeyTimeout)
	defer cancel()
	answer, err := e.up.Call(fetchCtx, plan.route.Method, plan.upstreamPath(), e.vars, nil)
	if err != nil {
		out.Verdict = CheckUnreachable
		out.Detail = err.Error()
		return out
	}
	switch {
	case e.up.Transient(answer):
		out.Verdict = CheckUnreachable
		out.Detail = fmt.Sprintf("the upstream answered %d, which says nothing about the stored row", answer.Status)
		return out
	case answer.Status == 404:
		out.Verdict = CheckGone
		out.Detail = "the upstream no longer has this"
		return out
	case answer.Status < 200 || answer.Status >= 300:
		out.Verdict = CheckUnreachable
		out.Detail = fmt.Sprintf("the upstream answered %d", answer.Status)
		return out
	}

	fresh, err := e.rowFromAnswer(plan, answer)
	if err != nil {
		out.Verdict = CheckUnreachable
		out.Detail = err.Error()
		return out
	}

	out.Diffs = diffRows(plan.res, stored, fresh)
	if len(out.Diffs) == 0 {
		out.Verdict = CheckAgrees
		return out
	}

	// A delivery that landed mid-fetch is a write, not drift. Re-reading the
	// bookkeeping is what tells the two apart, and calling a race drift would
	// send an operator hunting a bug that just corrected itself.
	if after, err := e.store.Freshness(ctx, kind, f.Key); err == nil && after != nil &&
		after.ChangedAt.After(f.ChangedAt) {
		out.Verdict = CheckRaced
		out.Detail = "the row changed while this key was being fetched"
		return out
	}

	out.Verdict = CheckDrifted
	if repair {
		if err := e.absorbAnswerRow(ctx, plan, fresh); err != nil {
			out.Detail = "could not write the upstream's answer: " + err.Error()
			return out
		}
		out.Repaired = true
	}
	return out
}

// rowFromAnswer projects an upstream answer onto the resource's columns without
// storing it. The comparison has to be against what WOULD be stored, not
// against the raw document: a field the spec never models is not drift.
func (e *Engine) rowFromAnswer(plan *fetchPlan, answer *Answer) (Row, error) {
	if answer.Overflow {
		return nil, fmt.Errorf("the upstream answered more than the %d byte cap", maxBodyBytes)
	}
	doc, err := decodeJSON(answer.Body)
	if err != nil {
		return nil, err
	}
	if plan.res.Store == StoreDocument {
		body, err := marshalJSON(trim(plan.res, doc))
		if err != nil {
			return nil, err
		}
		row := Row{"document": string(body)}
		for k, v := range plan.key {
			row[k] = v
		}
		return row, nil
	}
	row, err := absorb(plan.res, trim(plan.res, doc))
	if err != nil {
		return nil, err
	}
	for k, v := range plan.key {
		row[k] = v
	}
	return row, requireKeys(plan.res, row)
}

// absorbAnswerRow writes a row the check already projected.
func (e *Engine) absorbAnswerRow(ctx context.Context, plan *fetchPlan, row Row) error {
	return e.store.Put(ctx, plan.res, row, time.Now())
}

// diffRows compares the columns the spec declares, in declaration order.
//
// Only declared columns: bookkeeping the store adds is the mirror's own and has
// no upstream to disagree with.
func diffRows(res *Resource, stored, fresh Row) []Difference {
	var out []Difference
	names := make([]string, 0, len(res.Fields)+1)
	if res.Store == StoreDocument {
		names = append(names, "document")
	}
	for _, f := range res.Fields {
		names = append(names, f.Name)
	}
	for _, name := range names {
		was, now := displayValue(stored[name]), displayValue(fresh[name])
		if was != now {
			out = append(out, Difference{Field: name, Stored: was, Upstream: now})
		}
	}
	return out
}

// displayValue renders a column for comparison and for the page. Both sides go
// through it, so a difference is a real difference and not two spellings.
func displayValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

// CheckSummary tallies one run, so a page can say "12 keys, 2 drifted" without
// the reader counting rows.
type CheckSummary struct {
	Kind     string               `json:"kind"`
	Checked  int                  `json:"checked"`
	Verdicts map[CheckVerdict]int `json:"verdicts"`
	Repaired int                  `json:"repaired"`
	Keys     []KeyCheck           `json:"keys"`
	Elapsed  string               `json:"elapsed"`

	tally map[CheckVerdict]int
}

// add files one verdict into the summary.
func (s *CheckSummary) add(k KeyCheck) {
	if s.tally == nil {
		s.tally = map[CheckVerdict]int{}
	}
	s.tally[k.Verdict]++
	s.Checked++
	if k.Repaired {
		s.Repaired++
	}
	s.Keys = append(s.Keys, k)
}

// done seals the tally onto the wire field.
func (s *CheckSummary) done(started time.Time) {
	s.Verdicts = s.tally
	if s.Verdicts == nil {
		s.Verdicts = map[CheckVerdict]int{}
	}
	if s.Keys == nil {
		s.Keys = []KeyCheck{}
	}
	s.Elapsed = time.Since(started).Round(time.Millisecond).String()
}
