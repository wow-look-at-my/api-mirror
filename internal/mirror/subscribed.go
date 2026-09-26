package mirror

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Subscribed names the upstream call that lists which event types the
// provider sends.
type Subscribed struct {
	Method string
	Path   string
	// Field is the path to the list of event type names in the answer.
	Field string
	// Headers authenticate the call, rendered per call.
	Headers []Header
}

// subscribedTTL bounds how long the provider's answer is reused.
const subscribedTTL = 10 * time.Minute

// SubscribedView is what the dashboard shows. Missing is empty when the
// answer is unknown, and Error then says why: an unknown list must never
// read as every type missing.
type SubscribedView struct {
	Checked bool      `json:"checked"`
	Missing []string  `json:"missing"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

type subscribedCache struct {
	mu   sync.Mutex
	view SubscribedView
}

func buildSubscribed(n *node) (*Subscribed, error) {
	if err := checkAttrs(n, "method", "path", "field"); err != nil {
		return nil, err
	}
	s := &Subscribed{Method: strings.ToUpper(n.Attr("method")), Path: n.Attr("path"), Field: n.Attr("field")}
	if s.Method == "" {
		s.Method = http.MethodGet
	}
	for _, child := range n.Children() {
		if child.Name() != "header" {
			return nil, fmt.Errorf("<subscribed>: unexpected child element <%s>", child.Name())
		}
		h, err := buildHeader(child)
		if err != nil {
			return nil, err
		}
		s.Headers = append(s.Headers, h)
	}
	if !strings.HasPrefix(s.Path, "/") || s.Field == "" {
		return nil, fmt.Errorf("<subscribed> needs a path= starting with / and a field=")
	}
	return s, nil
}

// subscribedView reports the declared types the provider does not send.
func (e *Engine) subscribedView(ctx context.Context) SubscribedView {
	ev := e.spec.Events
	if ev == nil || ev.Subscribed == nil {
		return SubscribedView{}
	}
	c := &e.subscribed
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.view.At.IsZero() && time.Since(c.view.At) < subscribedTTL {
		return c.view
	}
	c.view = e.askSubscribed(ctx, ev)
	c.view.At = time.Now()
	return c.view
}

func (e *Engine) askSubscribed(ctx context.Context, ev *Events) SubscribedView {
	rule := ev.Subscribed
	set := http.Header{}
	for _, h := range rule.Headers {
		v, err := renderString(h.Value, e.vars)
		if err != nil {
			return SubscribedView{Error: "render " + h.Name + ": " + err.Error()}
		}
		if v != "" {
			set.Set(h.Name, v)
		}
	}
	ans, err := e.up.CallWith(withLane(ctx, LaneRefresh, "", "subscribed"), rule.Method, rule.Path, e.vars, nil, set, nil)
	if err != nil {
		return SubscribedView{Error: err.Error()}
	}
	if ans.Status != http.StatusOK {
		return SubscribedView{Error: fmt.Sprintf("%s answered %d", rule.Path, ans.Status)}
	}
	doc, err := decodeJSON(ans.Body)
	if err != nil {
		return SubscribedView{Error: err.Error()}
	}
	list, ok := lookupPath(doc, rule.Field).([]any)
	if !ok {
		return SubscribedView{Error: fmt.Sprintf("%s carries no list at %s", rule.Path, rule.Field)}
	}
	sent := make(map[string]struct{}, len(list))
	for _, v := range list {
		sent[scalarString(v)] = struct{}{}
	}
	view := SubscribedView{Checked: true, Missing: []string{}}
	seen := map[string]struct{}{}
	for _, d := range ev.List {
		if _, dup := seen[d.Type]; dup {
			continue
		}
		seen[d.Type] = struct{}{}
		if _, ok := sent[d.Type]; !ok {
			view.Missing = append(view.Missing, d.Type)
		}
	}
	sort.Strings(view.Missing)
	return view
}
