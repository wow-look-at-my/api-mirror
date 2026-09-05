package mirror

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/wow-look-at-my/api-mirror/internal/database/dbgen"
)

// What the operator surface reads, kept apart from the serving path's queries.

// browseLimit stops a page killing the server by being opened.
const browseLimit = 500

// StaleKey is key the sweep may bring up to date.
type StaleKey struct {
	Key       string
	ETag      string
	State     string
	ExpiresAt time.Time
}

// StaleKeys lists the keys of kind that have aged out, oldest.
func (s *Store) StaleKeys(ctx context.Context, kind string, now time.Time) ([]StaleKey, error) {
	rows, err := s.q.ListStaleKeys(ctx, dbgen.ListStaleKeysParams{
		Kind:      kind,
		ExpiresAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		Limit:     browseLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list stale %s: %w", kind, err)
	}
	out := make([]StaleKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, StaleKey{
			Key:       r.Key,
			ETag:      r.Etag,
			State:     r.State,
			ExpiresAt: unixTime(r.ExpiresAt),
		})
	}
	return out, nil
}

// KindStat is what kind holds.
type KindStat struct {
	Kind    string    `json:"kind"`
	Rows    int64     `json:"rows"`
	Errored int64     `json:"errored"`
	Newest  time.Time `json:"newest,omitempty"`
}

// KindStats reports every kind the cache holds rows for.
func (s *Store) KindStats(ctx context.Context) ([]KindStat, error) {
	rows, err := s.q.CountFreshnessByKind(ctx)
	if err != nil {
		return nil, fmt.Errorf("count kinds: %w", err)
	}
	out := make([]KindStat, 0, len(rows))
	for _, r := range rows {
		stat := KindStat{Kind: r.Kind, Rows: r.RowsHeld}
		if r.Errored.Valid {
			stat.Errored = int64(r.Errored.Float64)
		}
		if secs, ok := asUnix(r.Newest); ok && secs > 0 {
			stat.Newest = time.Unix(secs, 0).UTC()
		}
		out = append(out, stat)
	}
	return out, nil
}

// FreshnessByKind lists kind's keys and where each stands.
func (s *Store) FreshnessByKind(ctx context.Context, kind string) ([]Freshness, error) {
	rows, err := s.q.ListFreshnessByKind(ctx, dbgen.ListFreshnessByKindParams{Kind: kind, Limit: browseLimit})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	out := make([]Freshness, 0, len(rows))
	for _, r := range rows {
		out = append(out, Freshness{
			Kind:       r.Kind,
			Key:        r.Key,
			FetchedAt:  unixTime(r.FetchedAt),
			ChangedAt:  unixTime(r.ChangedAt),
			ETag:       r.Etag,
			ExpiresAt:  unixTime(r.ExpiresAt),
			State:      r.State,
			Error:      r.Error,
			RetryAfter: unixTime(r.RetryAfter),
			Status:     int(r.Status),
		})
	}
	return out, nil
}

// PrincipalStanding is caller and how much they have proven.
type PrincipalStanding struct {
	Principal string    `json:"principal"`
	Grants    int64     `json:"grants"`
	Newest    time.Time `json:"newest,omitempty"`
}

// Principals lists who currently holds proof, most proven.
func (s *Store) Principals(ctx context.Context, now time.Time) ([]PrincipalStanding, error) {
	rows, err := s.q.ListGrantPrincipals(ctx, dbgen.ListGrantPrincipalsParams{
		ExpiresAt: now.Unix(),
		Limit:     browseLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list principals: %w", err)
	}
	out := make([]PrincipalStanding, 0, len(rows))
	for _, r := range rows {
		p := PrincipalStanding{Principal: r.Principal, Grants: r.Grants}
		if secs, ok := asUnix(r.Newest); ok {
			p.Newest = time.Unix(secs, 0).UTC()
		}
		out = append(out, p)
	}
	return out, nil
}

// Standing is principal's whole reveal-layer position: what they have
// proven and what they have been refused.
type Standing struct {
	Principal string       `json:"principal"`
	Grants    []GrantEntry `json:"grants"`
	Denials   []DenyEntry  `json:"denials"`
}

// GrantEntry is proven access.
type GrantEntry struct {
	Resource  string    `json:"resource"`
	Key       string    `json:"key"`
	Source    string    `json:"source"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DenyEntry is remembered refusal.
type DenyEntry struct {
	Resource  string    `json:"resource"`
	Key       string    `json:"key"`
	Status    int       `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// StandingOf reports principal's grants and denials.
func (s *Store) StandingOf(ctx context.Context, principal string, now time.Time) (Standing, error) {
	out := Standing{Principal: principal, Grants: []GrantEntry{}, Denials: []DenyEntry{}}
	grants, err := s.q.ListGrantsByPrincipal(ctx, dbgen.ListGrantsByPrincipalParams{
		Principal: principal, ExpiresAt: now.Unix(), Limit: browseLimit,
	})
	if err != nil {
		return out, fmt.Errorf("grants of %s: %w", principal, err)
	}
	for _, g := range grants {
		out.Grants = append(out.Grants, GrantEntry{
			Resource:  g.Resource,
			Key:       g.Key,
			Source:    g.Source,
			ExpiresAt: time.Unix(g.ExpiresAt, 0).UTC(),
		})
	}
	denials, err := s.q.ListDenialsByPrincipal(ctx, dbgen.ListDenialsByPrincipalParams{
		Principal: principal, ExpiresAt: now.Unix(), Limit: browseLimit,
	})
	if err != nil {
		return out, fmt.Errorf("denials of %s: %w", principal, err)
	}
	for _, d := range denials {
		out.Denials = append(out.Denials, DenyEntry{
			Resource:  d.Resource,
			Key:       d.Key,
			Status:    int(d.Status),
			ExpiresAt: time.Unix(d.ExpiresAt, 0).UTC(),
		})
	}
	return out, nil
}

// CountDenials is how many refusals are still being replayed without asking.
func (s *Store) CountDenials(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.CountLiveDenials(ctx, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("count denials: %w", err)
	}
	return n, nil
}

// Rows reads resource's stored rows for the truth browser.
//
// The cap is applied here rather than in SQL because List is the read path
// a resource table has, and a would be a set of rules about
// what a row means.
func (s *Store) Rows(ctx context.Context, res *Resource, limit int) ([]Row, bool, error) {
	if limit <= 0 || limit > browseLimit {
		limit = browseLimit
	}
	rows, err := s.List(ctx, res, nil)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		// Reported, never silent: 500 quiet rows of 40,000 read as all of them.
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// asUnix reads a MAX() out of SQLite. The driver answers an aggregate as int64
// or float64 depending on the column, so both are read.
func asUnix(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
