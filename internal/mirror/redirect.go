package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Answers that are not JSON.
//
// A redirect resource keeps the Location of a 3xx answer. The Location is
// usually a signed URL with a short life, so a row lives only until that URL
// expires, less a margin. A text resource keeps a body that is not JSON, such
// as a diff, with its media type. Neither is rebuilt: the mirror has nothing
// to parse in either.

func buildRedirect(n *node) (*Redirect, error) {
	if err := checkAttrs(n, "margin", "unsigned", "fallback"); err != nil {
		return nil, err
	}
	rd := &Redirect{}
	for _, d := range []struct {
		attr string
		dst  *time.Duration
	}{{"margin", &rd.Margin}, {"unsigned", &rd.Unsigned}, {"fallback", &rd.Fallback}} {
		if v := n.Attr(d.attr); v != "" {
			parsed, err := parseDuration("<redirect "+d.attr+">", v)
			if err != nil {
				return nil, err
			}
			*d.dst = parsed
		}
	}
	for _, child := range n.Children() {
		if child.Name() != "expires" {
			return nil, fmt.Errorf("<redirect>: unexpected child element <%s>", child.Name())
		}
		if err := checkAttrs(child, "param", "format", "since"); err != nil {
			return nil, err
		}
		rd.Expires = append(rd.Expires, ExpiryParam{
			Param:  child.Attr("param"),
			Format: child.Attr("format"),
			Since:  child.Attr("since"),
		})
	}
	return rd, nil
}

// validateRedirect checks a resource's <redirect>. A redirect resource with
// no rule for how long a Location lives would keep a signed URL past its
// expiry. It serve a link that no longer opens.
func (r *Resource) validateRedirect() error {
	if r.Store != StoreRedirect {
		if r.Redirect != nil {
			return fmt.Errorf("resource %q: <redirect> describes a redirect resource, and this one stores a %s", r.Name, r.Store)
		}
		return nil
	}
	rd := r.Redirect
	if rd == nil {
		return fmt.Errorf("resource %q stores a redirect and declares no <redirect> saying how long a Location lives", r.Name)
	}
	if len(rd.Expires) == 0 && rd.Unsigned == 0 && rd.Fallback == 0 {
		return fmt.Errorf("resource %q: <redirect> names no <expires>, unsigned= or fallback=, so no Location could ever be kept", r.Name)
	}
	for _, x := range rd.Expires {
		if x.Param == "" {
			return fmt.Errorf("resource %q: <expires> needs a param", r.Name)
		}
		switch x.Format {
		case "", "time", "unix":
			if x.Since != "" {
				return fmt.Errorf("resource %q: <expires param=%q> since= goes with format=\"seconds\"", r.Name, x.Param)
			}
		case "seconds":
			if x.Since == "" {
				return fmt.Errorf("resource %q: <expires param=%q format=\"seconds\"> needs since= naming the signing time", r.Name, x.Param)
			}
		default:
			return fmt.Errorf("resource %q: <expires param=%q> format %q is not time, unix or seconds", r.Name, x.Param, x.Format)
		}
	}
	return nil
}

// validateHeld refuses route features a redirect or text answer cannot have.
// A text route must name its media type: the fetch asks the upstream for it.
func (rt *Route) validateHeld(res *Resource) error {
	if res.Store.answersJSON() {
		return nil
	}
	if rt.List || rt.BodyKey != "" || len(rt.Spreads) > 0 {
		return fmt.Errorf("route %s: a %s answer is one opaque row, so list, body-key and <spread> do not apply", rt.Path, res.Store)
	}
	for _, q := range rt.Query {
		if q.Empties != "" {
			return fmt.Errorf("route %s: empties= edits a JSON answer, and this route answers a %s", rt.Path, res.Store)
		}
	}
	if res.Store == StoreText && len(rt.Accept) == 0 {
		return fmt.Errorf("route %s: a text route needs <accept> naming the media type to ask the upstream for", rt.Path)
	}
	return nil
}

// lifetime is how long a Location may be served from now. It reports false
// when the rule keeps none.
func (rd *Redirect) lifetime(loc *url.URL, now time.Time) (time.Duration, bool) {
	q := loc.Query()
	for _, x := range rd.Expires {
		at, ok := x.at(q)
		if !ok {
			continue
		}
		ttl := at.Sub(now) - rd.Margin
		return ttl, ttl > 0
	}
	if loc.RawQuery == "" && rd.Unsigned > 0 {
		return rd.Unsigned, true
	}
	if rd.Fallback > 0 {
		return rd.Fallback, true
	}
	return 0, false
}

// at reads the expiry one parameter states. A value that does not parse
// states nothing.
func (x ExpiryParam) at(q url.Values) (time.Time, bool) {
	v := q.Get(x.Param)
	if v == "" {
		return time.Time{}, false
	}
	switch x.Format {
	case "unix":
		n, err := strconv.ParseInt(v, 10, 64)
		return time.Unix(n, 0), err == nil
	case "seconds":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		since, err := time.Parse("20060102T150405Z", q.Get(x.Since))
		if err != nil {
			return time.Time{}, false
		}
		return since.Add(time.Duration(n) * time.Second), true
	default:
		t, err := time.Parse(time.RFC3339, v)
		return t, err == nil
	}
}

// isRedirect reports whether a status sends the caller elsewhere.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// absorbHeld stores a redirect or text answer. It reports false when the
// answer is not one this resource holds; the caller then treats it like any
// other answer.
func (e *Engine) absorbHeld(ctx context.Context, plan *fetchPlan, a *Answer) (FetchResult, bool, error) {
	res := plan.res
	switch {
	case res.Store == StoreRedirect && isRedirect(a.Status):
		loc, err := e.baseURL.Parse(a.Header.Get("Location"))
		if err != nil || a.Header.Get("Location") == "" {
			return FetchResult{}, true, fmt.Errorf("upstream %s answered %d with no usable Location", plan.path, a.Status)
		}
		ttl, ok := res.Redirect.lifetime(loc, time.Now())
		if !ok {
			// A Location the rule cannot date is served to this caller and kept by nobody.
			return FetchResult{}, true, &UnstoredAnswer{Path: plan.path, Answer: a}
		}
		if res.TTL > 0 && res.TTL < ttl {
			ttl = res.TTL
		}
		if err := e.putHeld(ctx, plan, Row{colLocation: loc.String()}); err != nil {
			return FetchResult{}, true, err
		}
		return FetchResult{Changed: true, Status: http.StatusFound, TTL: ttl}, true, nil
	case res.Store == StoreText && a.Status >= 200 && a.Status < 300:
		media := a.Header.Get("Content-Type")
		if media == "" {
			media = plan.route.Accept[0]
		}
		if err := e.putHeld(ctx, plan, Row{colBody: string(a.Body), colMedia: media}); err != nil {
			return FetchResult{}, true, err
		}
		return FetchResult{ETag: a.Header.Get("ETag"), Changed: true, Status: a.Status}, true, nil
	case a.Status >= 200 && a.Status < 300:
		// A redirect resource answered with a body: nothing here can hold it.
		return FetchResult{}, true, &UnstoredAnswer{Path: plan.path, Answer: a}
	}
	return FetchResult{}, false, nil
}

func (e *Engine) putHeld(ctx context.Context, plan *fetchPlan, row Row) error {
	for k, v := range plan.key {
		row[k] = v
	}
	return e.store.Put(ctx, plan.res, row, time.Now())
}

// serveHeld answers from a stored redirect or text row.
func (e *Engine) serveHeld(ctx context.Context, w *recorder, m *match, res *Resource, outcome Outcome) {
	row, err := e.store.Get(ctx, res, m.key)
	if err != nil {
		w.note(DispError, "", "", "read-cache")
		http.Error(w, "read cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if row == nil {
		w.note(DispRefusal, "", "", "empty")
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	e.writeHeld(w, res, row, outcome)
}

// writeHeld serves a stored redirect or text row.
func (e *Engine) writeHeld(w *recorder, res *Resource, row Row, outcome Outcome) {
	w.Header().Set(e.hdr.Cache, string(outcome))
	switch res.Store {
	case StoreRedirect:
		loc, _ := row[colLocation].(string)
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	case StoreText:
		media, _ := row[colMedia].(string)
		body, _ := row[colBody].(string)
		w.Header().Set("Content-Type", media)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			logf("write %s: %v", res.Name, err)
		}
	}
}
