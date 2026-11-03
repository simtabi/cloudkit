package cloudkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// The RetryPolicy defaults, used for any field left zero.
const (
	DefaultMaxAttempts = 3
	DefaultBaseDelay   = 500 * time.Millisecond
	DefaultMaxDelay    = 30 * time.Second
)

// RetryPolicy says how the registry's clients retry. The zero value means 3
// tries, jittered exponential backoff from 500 ms, and Retry-After honoured up
// to 30 s. MaxAttempts 1 turns retries off.
//
// What is retried: a 429 for any request whose body can be replayed (the
// provider did not act on it), and a 502, 503 or 504 or a transport error
// only for idempotent methods (GET, HEAD, OPTIONS, PUT, DELETE), so a create
// is never sent twice. A 429 that outlasts the budget, or asks to wait longer
// than MaxDelay, ends in a [*RateLimitError].
type RetryPolicy struct {
	MaxAttempts int           // total tries, the first included; 0 means 3
	BaseDelay   time.Duration // first backoff; 0 means 500 ms
	MaxDelay    time.Duration // longest single wait, Retry-After included; 0 means 30 s
}

// Validate refuses negative values and a BaseDelay above MaxDelay.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 0 || p.BaseDelay < 0 || p.MaxDelay < 0 {
		return errors.New("cloudkit: retry policy: values must not be negative")
	}
	if p.baseDelay() > p.maxDelay() {
		return errors.New("cloudkit: retry policy: BaseDelay is above MaxDelay")
	}
	return nil
}

func (p RetryPolicy) attempts() int {
	if p.MaxAttempts == 0 {
		return DefaultMaxAttempts
	}
	return p.MaxAttempts
}

func (p RetryPolicy) baseDelay() time.Duration {
	if p.BaseDelay == 0 {
		return DefaultBaseDelay
	}
	return p.BaseDelay
}

func (p RetryPolicy) maxDelay() time.Duration {
	if p.MaxDelay == 0 {
		return DefaultMaxDelay
	}
	return p.MaxDelay
}

// Backoff returns the wait before try n+1 after try n failed (n from 1): the
// base doubled per try, capped at MaxDelay, then jittered into its upper half.
func (p RetryPolicy) Backoff(n int) time.Duration {
	d := p.baseDelay()
	for i := 1; i < n && d < p.maxDelay(); i++ {
		d *= 2
	}
	d = min(d, p.maxDelay())
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not a secret
}

// RateLimitError is returned when the provider kept throttling past the retry
// budget. It matches [ErrRateLimited].
type RateLimitError struct {
	RetryAfter time.Duration // what the provider last asked for; 0 if it did not say
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("cloudkit: rate limited: retry after %s", e.RetryAfter)
	}
	return "cloudkit: rate limited"
}

// Unwrap lets errors.Is match [ErrRateLimited].
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type retrier struct {
	next   http.RoundTripper
	policy RetryPolicy
	sleep  func(ctx context.Context, d time.Duration) error
}

func (r *retrier) RoundTrip(req *http.Request) (*http.Response, error) {
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	idempotent := isIdempotent(req.Method)
	attempts := r.policy.attempts()
	for try := 1; ; try++ {
		attempt := req
		if try > 1 {
			var err error
			if attempt, err = rewind(req); err != nil {
				return nil, err
			}
		}
		resp, err := r.next.RoundTrip(attempt)
		last := try >= attempts
		var wait time.Duration
		switch {
		case err != nil:
			if last || !idempotent || !replayable || req.Context().Err() != nil {
				return nil, err
			}
			wait = r.policy.Backoff(try)
		case resp.StatusCode == http.StatusTooManyRequests:
			after, said := retryAfter(resp, time.Now())
			discard(resp)
			if last || !replayable || after > r.policy.maxDelay() {
				return nil, &RateLimitError{RetryAfter: after}
			}
			wait = r.policy.Backoff(try)
			if said {
				wait = after
			}
		case transient(resp.StatusCode):
			if last || !idempotent || !replayable {
				return resp, nil
			}
			after, said := retryAfter(resp, time.Now())
			if said && after > r.policy.maxDelay() {
				return resp, nil
			}
			discard(resp)
			wait = r.policy.Backoff(try)
			if said {
				wait = after
			}
		default:
			return resp, nil
		}
		if err := r.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
}

func transient(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func isIdempotent(method string) bool {
	switch method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// rewind clones req with a fresh body for another try.
func rewind(req *http.Request) (*http.Request, error) {
	out := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		out.Body = body
	}
	return out, nil
}

// retryAfter reads Retry-After as seconds or an HTTP date. said is false when
// the header is absent or unreadable.
func retryAfter(resp *http.Response, now time.Time) (d time.Duration, said bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil {
		return max(time.Duration(s)*time.Second, 0), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// discard drains a little of a body, so the connection can be reused, and
// closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
