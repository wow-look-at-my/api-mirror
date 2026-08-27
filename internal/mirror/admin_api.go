package mirror

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The dashboard's JSON. One handler per tab, each answering the whole of what
// that tab shows so the page makes one request per view rather than stitching
// several together.

// writeJSON renders one admin answer.
//
// It marshals. A JSON literal built with string concatenation guesses about
// every value it interpolates, and a path or a subject carries whatever the
// upstream put in it.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalJSON(v)
	if err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		logf("dashboard: write: %v", err)
	}
}

// RequestsView is the Requests tab.
type RequestsView struct {
	Recent []Request       `json:"recent"`
	Groups []*RequestGroup `json:"groups"`
	// Means: a total with no average hides one slow shape inside a busy one.
	Means map[string]int64 `json:"means_ns"`
}

func (a *Admin) requests(w http.ResponseWriter, r *http.Request) {
	groups := a.engine.tel.Requests.Groups()
	means := make(map[string]int64, len(groups))
	for _, g := range groups {
		means[g.Method+" "+g.Shape] = int64(g.MeanDuration())
	}
	writeJSON(w, http.StatusOK, RequestsView{
		Recent: a.engine.tel.Requests.Recent(intParam(r, "limit", 200)),
		Groups: groups,
		Means:  means,
	})
}

// TimelineView is the Timeline tab: the frames and what the ring can say about
// itself, including what it dropped.
type TimelineView struct {
	Frames []Frame       `json:"frames"`
	Stats  TimelineStats `json:"stats"`
}

func (a *Admin) timeline(w http.ResponseWriter, r *http.Request) {
	frames := a.engine.tel.Timeline.Frames()
	if lane := r.URL.Query().Get("lane"); lane != "" {
		filtered := frames[:0:0]
		for _, f := range frames {
			if string(f.Lane) == lane {
				filtered = append(filtered, f)
			}
		}
		frames = filtered
	}
	writeJSON(w, http.StatusOK, TimelineView{Frames: frames, Stats: a.engine.tel.Timeline.Stats()})
}

// RatesView is the Rate limit tab.
type RatesView struct {
	Budgets []RateBudget `json:"budgets"`
	// Declared separates "no traffic yet" from "cannot read a budget at all".
	Declared bool        `json:"declared"`
	Headers  RateHeaders `json:"headers"`
}

func (a *Admin) rates(w http.ResponseWriter, _ *http.Request) {
	h := a.engine.spec.Upstream.Rate
	writeJSON(w, http.StatusOK, RatesView{
		Budgets:  a.engine.tel.Rates.Snapshot(),
		Declared: h.declared(),
		Headers:  h,
	})
}

// BriefView is the implementation brief: what is still leaving, worst first.
type BriefView struct {
	Items []BriefItem `json:"items"`
	Total int         `json:"total"`
}

func (a *Admin) brief(w http.ResponseWriter, _ *http.Request) {
	items := a.engine.Brief()
	total := 0
	for _, it := range items {
		total += it.Count
	}
	writeJSON(w, http.StatusOK, BriefView{Items: items, Total: total})
}

// PrincipalsView is the Principals tab: who has proven what.
type PrincipalsView struct {
	Principals []PrincipalStanding `json:"principals"`
	Standing   *Standing           `json:"standing,omitempty"`
	Denials    int64               `json:"denials"`
}

func (a *Admin) principals(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	view := PrincipalsView{}
	list, err := a.engine.store.Principals(r.Context(), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.Principals = list
	if n, err := a.engine.store.CountDenials(r.Context(), now); err == nil {
		view.Denials = n
	}
	if who := r.URL.Query().Get("principal"); who != "" {
		standing, err := a.engine.store.StandingOf(r.Context(), who, now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		view.Standing = &standing
	}
	writeJSON(w, http.StatusOK, view)
}

// ResourceView describes one declared resource and, on request, its rows.
type ResourceView struct {
	Name      string      `json:"name"`
	Store     StoreMode   `json:"store"`
	TTL       string      `json:"ttl,omitempty"`
	Keys      []string    `json:"keys"`
	Fields    []FieldView `json:"fields"`
	Routes    []string    `json:"routes"`
	Reveal    RevealView  `json:"reveal"`
	Rows      []Row       `json:"rows,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

// FieldView is one stored column as the page shows it.
type FieldView struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`
	From string    `json:"from,omitempty"`
}

// RevealView is how a resource is gated, in the words the ladder uses.
type RevealView struct {
	Public     string `json:"public,omitempty"`
	Probe      string `json:"probe,omitempty"`
	GrantTTL   string `json:"grant_ttl,omitempty"`
	DenyTTL    string `json:"deny_ttl,omitempty"`
	Credential bool   `json:"credential,omitempty"`
}

func (a *Admin) resources(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("resource")
	out := make([]ResourceView, 0, len(a.engine.spec.Resources))
	for _, res := range a.engine.spec.Resources {
		view := describeResource(a.engine, res)
		if want != "" && res.Name == want {
			rows, truncated, err := a.engine.store.Rows(r.Context(), res, intParam(r, "limit", browseLimit))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			view.Rows = rows
			view.Truncated = truncated
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func describeResource(e *Engine, res *Resource) ResourceView {
	view := ResourceView{Name: res.Name, Store: res.Store, Keys: []string{}, Fields: []FieldView{}, Routes: []string{}}
	if res.TTL > 0 {
		view.TTL = res.TTL.String()
	}
	for _, k := range res.Keys {
		name := k.Name
		if k.Credential {
			name += " (credential)"
		}
		view.Keys = append(view.Keys, name)
	}
	for _, f := range res.Fields {
		view.Fields = append(view.Fields, FieldView{Name: f.Name, Type: f.Type, From: f.From})
	}
	for _, rt := range e.spec.Routes {
		if rt.Resource == res.Name {
			view.Routes = append(view.Routes, rt.Method+" "+rt.Path)
		}
	}
	if res.Reveal != nil {
		view.Reveal = RevealView{Public: res.Reveal.Public, Credential: res.Reveal.Credential}
		if res.Reveal.Probe != nil {
			view.Reveal.Probe = res.Reveal.Probe.Method + " " + res.Reveal.Probe.Path
		}
		if res.Reveal.GrantTTL > 0 {
			view.Reveal.GrantTTL = res.Reveal.GrantTTL.String()
		}
		if res.Reveal.DenyTTL > 0 {
			view.Reveal.DenyTTL = res.Reveal.DenyTTL.String()
		}
	}
	return view
}

// EventsView is the Webhooks tab.
type EventsView struct {
	Path       string        `json:"path,omitempty"`
	Stats      DeliveryStats `json:"stats"`
	Declared   []EventView   `json:"declared"`
	Replay     ReplayStats   `json:"replay"`
	Configured bool          `json:"configured"`
}

// EventView is one declared event and how it is ordered.
type EventView struct {
	Type      string `json:"type"`
	Resource  string `json:"resource"`
	Clock     string `json:"clock,omitempty"`
	Unordered bool   `json:"unordered,omitempty"`
	Sets      int    `json:"sets"`
	// Invalidate is the escape hatch's stated reason; empty means it applies.
	Invalidate string `json:"invalidate,omitempty"`
}

func (a *Admin) events(w http.ResponseWriter, _ *http.Request) {
	view := EventsView{Replay: a.engine.replay.Stats(), Declared: []EventView{}}
	if a.engine.spec.Events == nil {
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Configured = a.engine.ingest != nil
	view.Path = a.engine.spec.Events.Path
	view.Stats = a.engine.ingest.Stats()
	for _, ev := range a.engine.spec.Events.List {
		e := EventView{
			Type:      ev.Type,
			Resource:  ev.Resource,
			Clock:     ev.Clock,
			Unordered: ev.Unordered,
			Sets:      len(ev.Sets),
		}
		if ev.Invalidate != nil {
			e.Invalidate = ev.Invalidate.Reason
		}
		view.Declared = append(view.Declared, e)
	}
	writeJSON(w, http.StatusOK, view)
}

// SpecView is the derived schema, as --check prints it.
type SpecView struct {
	Fingerprint string `json:"fingerprint"`
	DDL         string `json:"ddl"`
}

func (a *Admin) specReport(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, SpecView{
		Fingerprint: a.engine.spec.Fingerprint(),
		DDL:         a.engine.spec.DDL(),
	})
}

// runRefresh triggers one sweep by hand, for an operator who does not want to
// wait for the next tick.
func (a *Admin) runRefresh(w http.ResponseWriter, _ *http.Request) {
	if !a.engine.refresh.Enabled() {
		http.Error(w, "this spec declares no <refresh>", http.StatusNotImplemented)
		return
	}
	go a.engine.refresh.cycle()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sweeping"})
}

func intParam(r *http.Request, name string, fallback int) int {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
