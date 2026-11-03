package cloudkit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// do sends one request through the harness's guarded client.
func do(t *testing.T, h *harness, method, path, body string) (int, error) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "https://"+fakeHost+path, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.open().acct.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func TestRetryHonoursRetryAfterOn429(t *testing.T) {
	h := newHarness(t, Options{})
	h.world.fail = []int{http.StatusTooManyRequests}
	code, err := do(t, h, http.MethodGet, "/v1/resources", "")
	if err != nil || code != http.StatusOK {
		t.Fatalf("got %d, %v", code, err)
	}
	if len(h.slept) != 1 || h.slept[0] != 2*time.Second {
		t.Fatalf("slept %v, want [2s] from Retry-After", h.slept)
	}
	if h.creds.count() != 2 {
		t.Fatalf("token fetched %d times, want once per try", h.creds.count())
	}
}

func TestRetryGivesUpWithRateLimitError(t *testing.T) {
	h := newHarness(t, Options{})
	h.world.fail = []int{429, 429, 429, 429}
	_, err := do(t, h, http.MethodGet, "/v1/resources", "")
	var rl *RateLimitError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second {
		t.Fatalf("got %v, want a RateLimitError with RetryAfter 2s", err)
	}
	if n := len(h.world.requests()); n != DefaultMaxAttempts {
		t.Fatalf("%d tries, want %d", n, DefaultMaxAttempts)
	}
}

func TestRetryDoesNotWaitLongerThanMaxDelay(t *testing.T) {
	h := newHarness(t, Options{Retry: RetryPolicy{MaxDelay: time.Second, BaseDelay: time.Millisecond}})
	h.world.fail = []int{429} // asks for 2s
	_, err := do(t, h, http.MethodGet, "/v1/resources", "")
	if !errors.Is(err, ErrRateLimited) || len(h.slept) != 0 || len(h.world.requests()) != 1 {
		t.Fatalf("got %v after %d tries and sleeps %v", err, len(h.world.requests()), h.slept)
	}
}

// A create is never sent twice on an ambiguous failure, but a 429 (not acted
// on) is retried with its body intact.
func TestRetryNeverRepeatsAnAmbiguousWrite(t *testing.T) {
	h := newHarness(t, Options{})
	h.world.fail = []int{http.StatusServiceUnavailable}
	code, err := do(t, h, http.MethodPost, "/v1/resources", `{"kind":"server","name":"web-1"}`)
	if err != nil || code != http.StatusServiceUnavailable || len(h.world.requests()) != 1 {
		t.Fatalf("POST after a 503: code %d, err %v, %d tries", code, err, len(h.world.requests()))
	}

	h = newHarness(t, Options{})
	h.world.fail = []int{http.StatusTooManyRequests}
	body := `{"kind":"server","name":"web-1"}`
	code, err = do(t, h, http.MethodPost, "/v1/resources", body)
	reqs := h.world.requests()
	if err != nil || code != http.StatusCreated || len(reqs) != 2 || reqs[1].Body != body {
		t.Fatalf("POST after a 429: code %d, err %v, tries %+v", code, err, reqs)
	}
}

func TestRetryTransientErrorsOnIdempotentMethods(t *testing.T) {
	for _, code := range []int{502, 503, 504} {
		h := newHarness(t, Options{})
		h.world.fail = []int{code, code}
		got, err := do(t, h, http.MethodGet, "/v1/resources", "")
		if err != nil || got != http.StatusOK || len(h.world.requests()) != 3 {
			t.Errorf("%d: got %d, %v after %d tries", code, got, err, len(h.world.requests()))
		}
	}
	h := newHarness(t, Options{})
	h.world.fail = []int{500}
	if got, _ := do(t, h, http.MethodGet, "/v1/resources", ""); got != 500 || len(h.world.requests()) != 1 {
		t.Errorf("a 500 was retried")
	}
}

func TestRetryTransportErrors(t *testing.T) {
	flaky := func(fails int) (*int, roundTripFunc) {
		tries := 0
		return &tries, func(req *http.Request) (*http.Response, error) {
			tries++
			if req.Body != nil {
				_ = req.Body.Close()
			}
			if tries <= fails {
				return nil, errors.New("connection reset")
			}
			return reply(req, http.StatusOK, nil), nil
		}
	}
	for _, c := range []struct {
		method    string
		wantTries int
		wantOK    bool
	}{
		{http.MethodGet, 2, true},
		{http.MethodDelete, 2, true},
		{http.MethodPost, 1, false},
	} {
		tries, rt := flaky(1)
		h := newHarness(t, Options{Transport: Transport{HTTP: rt, Dial: noDial}})
		_, err := do(t, h, c.method, "/v1/resources", "")
		if (err == nil) != c.wantOK || *tries != c.wantTries {
			t.Errorf("%s: err %v after %d tries", c.method, err, *tries)
		}
	}
}

func TestRetryOffAndCancelled(t *testing.T) {
	h := newHarness(t, Options{Retry: RetryPolicy{MaxAttempts: 1}})
	h.world.fail = []int{503}
	if got, _ := do(t, h, http.MethodGet, "/v1/resources", ""); got != 503 || len(h.world.requests()) != 1 {
		t.Fatal("MaxAttempts 1 retried")
	}

	h = newHarness(t, Options{})
	h.reg.sleep = sleepCtx
	h.world.fail = []int{429}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+fakeHost+"/v1/resources", nil)
	resp, err := h.open().acct.HTTP.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a cancelled context kept retrying")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestRetryAfterForms(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"7":  7 * time.Second,
		"-3": 0,
		now.Add(time.Minute).Format(http.TimeFormat):  time.Minute,
		now.Add(-time.Minute).Format(http.TimeFormat): 0,
	} {
		resp := &http.Response{Header: http.Header{"Retry-After": {v}}}
		got, said := retryAfter(resp, now)
		if !said || got != want {
			t.Errorf("Retry-After %q = %s, %v; want %s", v, got, said, want)
		}
	}
	for _, v := range []string{"", "soon"} {
		if _, said := retryAfter(&http.Response{Header: http.Header{"Retry-After": {v}}}, now); said {
			t.Errorf("Retry-After %q was read", v)
		}
	}
}

func TestBackoffIsJitteredAndCapped(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	for n, ceil := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 400 * time.Millisecond, 10: time.Second} {
		for range 50 {
			d := p.Backoff(n)
			if d < ceil/2 || d > ceil {
				t.Fatalf("Backoff(%d) = %s, want within [%s, %s]", n, d, ceil/2, ceil)
			}
		}
	}
	var zero RetryPolicy
	if zero.attempts() != 3 || zero.baseDelay() != DefaultBaseDelay || zero.maxDelay() != DefaultMaxDelay || zero.Validate() != nil {
		t.Fatal("the zero RetryPolicy is not 3 tries with the defaults")
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
