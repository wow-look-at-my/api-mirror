// headers.go holds defaultHeaderPrefix.

package mirror

import "cmp"

// defaultHeaderPrefix starts every header the engine sets on an answer, unless the spec names another prefix.
const defaultHeaderPrefix = "X-Mirror-"

// answerHeaders are the names of the headers the engine sets on an answer.
type answerHeaders struct {
	Cache       string
	Reason      string
	Debounce    string
	Stale       string
	LastFetched string
	Disposition string
	// Event and Delivery are on a subscriber notification, not an answer.
	Event    string
	Delivery string
}

func headersFor(prefix string) answerHeaders {
	p := cmp.Or(prefix, defaultHeaderPrefix)
	return answerHeaders{
		Cache:       p + "Cache",
		Reason:      p + "Passthrough-Reason",
		Debounce:    p + "Debounce",
		Stale:       p + "Stale",
		LastFetched: p + "Last-Fetched",
		Disposition: p + "Disposition",
		Event:       p + "Event",
		Delivery:    p + "Delivery",
	}
}

// exposed lists what a browser must be allowed to read on an answer.
func (h answerHeaders) exposed() []string {
	return []string{h.Cache, h.Reason, h.Debounce, h.Stale, h.LastFetched}
}

// dispositionHeader is the default name, for tests of a spec that sets no prefix.
var dispositionHeader = headersFor("").Disposition
