package mirror

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// UnstoredAnswer is an upstream answer that the route does not keep.
type UnstoredAnswer struct {
	Path   string
	Answer *Answer
}

func (u *UnstoredAnswer) Error() string {
	return fmt.Sprintf("upstream %s answered %d, which this route does not store", u.Path, u.Answer.Status)
}

// Unstored tells freshness that this is one caller's answer and not an upstream fault.
func (u *UnstoredAnswer) Unstored() bool { return true }

// refusesAccess reports whether a status says "this caller may not read
// this". Only these statuses become a denial.
func refusesAccess(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// relayUnstored answers a caller with the upstream's own answer to the fetch
// their credential made.
//
// An access refusal is remembered as this principal's denial, exactly as a
// refused probe is, so the caller does not re-ask on every request. Any other
// status is reported as an answer the route does not model.
func (e *Engine) relayUnstored(ctx context.Context, w *recorder, m *match, res *Resource, principal string, u *UnstoredAnswer) {
	a := u.Answer
	if refusesAccess(a.Status) && !res.Reveal.Credential && res.Reveal.DenyTTL > 0 {
		// The denial is remembered under the proof the reveal ladder asks for.
		scope, k, err := e.reveal.proofOf(res, m.key)
		if err != nil {
			logf("relay %s: %v", u.Path, err)
		} else {
			e.reveal.remember(ctx, principal, scope, k, a.Status, e.reveal.now().Add(res.Reveal.DenyTTL))
		}
		w.note(DispDenied, "", "", "fetch-refused")
	} else {
		w.note(DispRelayed, "", "", strconv.Itoa(a.Status))
	}
	e.relayAnswer(w, a)
}
