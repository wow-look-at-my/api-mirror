// freshness.go holds fetchSafetyTimeout.

package mirror

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// fetchSafetyTimeout bounds a detached fetch. It is a leak guard.
const fetchSafetyTimeout = 5 * time.Minute

// defaultErrorRetry is how long a failed fetch is left alone when the spec
const defaultErrorRetry = time.Minute

// FetchState is where cached key stands.
type FetchState string

const (
	StateUnknown  FetchState = "unknown"
	StateFresh    FetchState = "fresh"
	StateStale    FetchState = "stale"
	StateFetching FetchState = "fetching"
	StateError    FetchState = "error"
)

// Outcome is what a read did to get its answer.
type Outcome string

const (
	OutcomeHit   Outcome = "hit"
	OutcomeMiss  Outcome = "miss"
	OutcomeError Outcome = "error"
)

// FetchResult is what a fetcher reports back.
type FetchResult struct {
	ETag    string
	Changed bool
	// Status is the upstream answer the route declared worth keeping.
	Status int
	// Body is the refusal document kept with a refusal status.
	Body string
	// TTL overrides the kind's TTL for this answer. Zero keeps the kind's.
	TTL time.Duration
}

// Fetcher performs refresh. The engine supplies it; this file knows nothing
type Fetcher func(ctx context.Context, kind, key, etag string) (FetchResult, error)

// Fresh keeps cached keys current.
//
// It holds properties that are easy to lose and expensive to debug.
type Fresh struct {
	store *Store
	fetch Fetcher
	ttl   func(kind string) time.Duration

	locks    sync.Map // key -> *sync.Mutex
	wg       sync.WaitGroup
	inflight atomic.Int64
	now      func() time.Time
}

// NewFresh returns a freshness manager. ttl resolves a kind's TTL, so the
// policy stays in the spec.
func NewFresh(store *Store, fetch Fetcher, ttl func(kind string) time.Duration) *Fresh {
	return &Fresh{store: store, fetch: fetch, ttl: ttl, now: time.Now}
}

// Busy reports whether a fetch is in flight. It answers immediately.
func (f *Fresh) Busy() bool { return f.inflight.Load() > 0 }

// Drain waits for in-flight fetches, so a shutdown does not close the database
// under of them.
func (f *Fresh) Drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Ensure brings key up to date and reports what it took.
//
// A fresh row returns immediately. A row inside its error backoff returns the
// STORED error rather than re-fetching. An upstream that is failing does not
// need every request to prove it again.
func (f *Fresh) Ensure(ctx context.Context, kind, key string) (Outcome, error) {
	meta, err := f.store.Freshness(ctx, kind, key)
	if err != nil {
		return OutcomeError, err
	}
	now := f.now()
	if meta != nil {
		if FetchState(meta.State) == StateFresh && meta.ExpiresAt.After(now) {
			return OutcomeHit, nil
		}
		if err := backoffError(meta, now); err != nil {
			return OutcomeError, err
		}
	}
	return f.doFetch(ctx, kind, key, meta)
}

// Refresh fetches whether the row is fresh, and ignores the error backoff. It
// is what a deliberate refresh means: the caller has decided.
func (f *Fresh) Refresh(ctx context.Context, kind, key string) (Outcome, error) {
	meta, err := f.store.Freshness(ctx, kind, key)
	if err != nil {
		return OutcomeError, err
	}
	return f.doFetch(ctx, kind, key, meta)
}

// backoffError replays a stored failure while its retry window is open.
func backoffError(meta *Freshness, now time.Time) error {
	if FetchState(meta.State) != StateError {
		return nil
	}
	if meta.RetryAfter.IsZero() || !now.Before(meta.RetryAfter) {
		return nil
	}
	return &StoredError{Kind: meta.Kind, Key: meta.Key, Message: meta.Error, Until: meta.RetryAfter}
}

// StoredError is a failure being replayed from the freshness row rather than
// re-earned from the upstream.
type StoredError struct {
	Kind, Key string
	Message   string
	Until     time.Time
}

func (e *StoredError) Error() string {
	return "upstream failed for " + e.Kind + "/" + e.Key + ", retrying after " +
		e.Until.Format(time.RFC3339) + ": " + e.Message
}

// doFetch runs refresh under this key's lock.
func (f *Fresh) doFetch(ctx context.Context, kind, key string, meta *Freshness) (Outcome, error) {
	// The fetch is detached from the caller's context. A consumer that hangs
	detached := context.WithoutCancel(ctx)
	fetchCtx, cancel := context.WithTimeout(detached, fetchSafetyTimeout)
	defer cancel()

	lock := f.lockFor(kind, key)
	lock.Lock()
	defer lock.Unlock()

	// Another caller may have refreshed this key while we waited for the lock.
	if fresh, err := f.store.Freshness(detached, kind, key); err == nil && fresh != nil {
		if FetchState(fresh.State) == StateFresh && fresh.ExpiresAt.After(f.now()) {
			return OutcomeHit, nil
		}
		meta = fresh
	}

	f.wg.Add(1)
	f.inflight.Add(1)
	defer func() {
		f.inflight.Add(-1)
		f.wg.Done()
	}()

	etag := ""
	if meta != nil {
		etag = meta.ETag
	}
	if err := f.store.MarkFetching(detached, kind, key); err != nil {
		logf("mark fetching %s/%s: %v", kind, key, err)
	}

	res, err := f.fetch(fetchCtx, kind, key, etag)
	now := f.now()
	var mine interface{ Unstored() bool }
	if errors.As(err, &mine) && mine.Unstored() {
		// One caller's answer: other callers must not read it as a backoff.
		f.restore(detached, kind, key, meta)
		return OutcomeError, err
	}
	if err != nil {
		retryAfter := now.Add(defaultErrorRetry)
		if werr := f.store.MarkError(detached, kind, key, err.Error(), retryAfter); werr != nil {
			logf("record error %s/%s: %v", kind, key, werr)
		}
		return OutcomeError, err
	}

	ttl := f.ttl(kind)
	if res.TTL > 0 {
		ttl = res.TTL
	}
	record := Freshness{
		Kind:      kind,
		Key:       key,
		FetchedAt: now,
		ETag:      res.ETag,
		ExpiresAt: now.Add(ttl),
		State:     string(StateFresh),
		Status:    res.Status,
	}
	if res.Changed {
		record.ChangedAt = now
	} else if meta != nil {
		record.ChangedAt = meta.ChangedAt
	}
	if res.Status >= 400 {
		if err := f.store.SetRefusalBody(detached, kind, key, res.Body); err != nil {
			return OutcomeError, err
		}
	}
	if err := f.store.RecordFetched(detached, record); err != nil {
		return OutcomeError, err
	}
	return OutcomeMiss, nil
}

// restore puts back the bookkeeping a fetch found, after a fetch whose answer
// was not the key's to keep.
func (f *Fresh) restore(ctx context.Context, kind, key string, meta *Freshness) {
	var err error
	switch {
	case meta == nil:
		_, err = f.store.DeleteFreshness(ctx, kind, key)
	case FetchState(meta.State) == StateError:
		err = f.store.MarkError(ctx, kind, key, meta.Error, meta.RetryAfter)
	default:
		err = f.store.RecordFetched(ctx, *meta)
	}
	if err != nil {
		logf("restore freshness %s/%s: %v", kind, key, err)
	}
}

// lockFor returns the mutex guarding key, creating it.
func (f *Fresh) lockFor(kind, key string) *sync.Mutex {
	actual, _ := f.locks.LoadOrStore(kind+"\x00"+key, &sync.Mutex{})
	return actual.(*sync.Mutex)
}
