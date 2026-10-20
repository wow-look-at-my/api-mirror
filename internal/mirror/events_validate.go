package mirror

import (
	"fmt"
	"strings"

	"github.com/wow-look-at-my/go-containers/set"
)

// validate refuses an <events> block that would accept a delivery it cannot
// apply, or would apply events to one row without saying which wins.
func (e *Events) validate(resources map[string]*Resource) error {
	if e.Path == "" {
		return fmt.Errorf("<events> needs a path")
	}
	if e.Secret == "" {
		return fmt.Errorf("<events> needs a secret: an unverified delivery is anyone's delivery")
	}
	if e.SignatureHeader == "" {
		return fmt.Errorf("<events> needs a signature header")
	}
	if e.TypeHeader == "" {
		return fmt.Errorf("<events> needs a type header")
	}
	// A type declared more than once for one resource has to be told apart by
	// its when=, or the later declaration silently decides the row.
	type repeat struct {
		typ, res     string
		declarations int
		bare         int
	}
	repeats := map[string]*repeat{}
	for _, ev := range e.List {
		pair := ev.Type + "\x00" + ev.Resource
		r := repeats[pair]
		if r == nil {
			r = &repeat{typ: ev.Type, res: ev.Resource}
			repeats[pair] = r
		}
		r.declarations++
		if ev.When == "" {
			r.bare++
		}
	}
	for _, r := range repeats {
		if r.declarations > 1 && r.bare > 0 {
			return fmt.Errorf("event %q declared twice for resource %q: two handlers for one type and resource need when= on every declaration", r.typ, r.res)
		}
	}
	// A type is declared per resource it moves.
	seen := set.New[string]()
	for _, ev := range e.List {
		if ev.Type == "" {
			return fmt.Errorf("<event> needs a type")
		}
		handler := ev.Type + "\x00" + ev.Resource + "\x00" + strings.Join(ev.Actions, ",") + "\x00" + ev.When
		if seen.Contains(handler) {
			return fmt.Errorf("event %q declared twice for resource %q", ev.Type, ev.Resource)
		}
		seen.Add(handler)
		res, ok := resources[ev.Resource]
		if !ok {
			return fmt.Errorf("event %q names resource %q, which is not declared", ev.Type, ev.Resource)
		}
		if ev.Subject == "" {
			return fmt.Errorf("event %q needs a <subject>: without one the engine cannot tell which deliveries order against each other", ev.Type)
		}
		if ev.Clock == "" && !ev.Unordered {
			return fmt.Errorf("event %q needs a <clock>, or must declare unordered=\"true\": a delivery whose moment is unknown can overwrite newer truth silently", ev.Type)
		}
		if ev.Clock != "" && ev.Unordered {
			return fmt.Errorf("event %q declares both a clock and unordered", ev.Type)
		}
		if len(ev.Sets) == 0 && ev.Invalidate == nil && ev.Document == "" {
			return fmt.Errorf("event %q does nothing: give it <set> fields, a <document>, or an <invalidate> with a reason", ev.Type)
		}
		if err := validateEventDocument(ev, res); err != nil {
			return err
		}
		if len(ev.Sets) > 0 && ev.Invalidate != nil {
			return fmt.Errorf("event %q both writes and invalidates; the write already replaced the row", ev.Type)
		}
		if ev.Invalidate != nil && strings.TrimSpace(ev.Invalidate.Reason) == "" {
			return fmt.Errorf("event %q: <invalidate> needs a reason stating why the payload cannot answer -- throwing away a value the upstream just handed us is the bug this asks you to justify", ev.Type)
		}
		if res.whole() && len(ev.Sets) > 0 {
			return fmt.Errorf("event %q writes fields into resource %q, which stores a document", ev.Type, res.Name)
		}
		known := set.New[string]()
		for _, f := range res.Fields {
			known.Add(f.Name)
		}
		for _, k := range res.Keys {
			known.Add(k.Name)
		}
		for _, st := range ev.Sets {
			if !known.Contains(st.Field) {
				return fmt.Errorf("event %q sets %q, which resource %q does not declare", ev.Type, st.Field, res.Name)
			}
			if (st.From == "") == (st.Expr == "") {
				return fmt.Errorf("event %q set %q: give it a payload path or an expr, not both and not neither", ev.Type, st.Field)
			}
		}
		if err := validateEventSources(ev); err != nil {
			return err
		}
		if err := validateEventKeys(ev, res); err != nil {
			return err
		}
	}
	return nil
}
