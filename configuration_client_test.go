package rollfuse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfigJSON(version int64) []byte {
	cfg := Configuration{
		EnvironmentID: "env_1",
		Version:       version,
		Flags: []FlagConfig{
			{
				FlagKey:          "checkout-redesign",
				Enabled:          true,
				DefaultVariation: "off",
				Variations: []Variation{
					{Key: "on", Value: json.RawMessage(`true`)},
					{Key: "off", Value: json.RawMessage(`false`)},
				},
			},
		},
	}

	body, _ := json.Marshal(cfg)

	return body
}

func TestConfigurationClient_StartResolvesOnFirstSuccess(t *testing.T) {
	var refreshedVersion atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer cred" {
			t.Errorf("expected Authorization header 'Bearer cred', got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testConfigJSON(3))
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		onConfigRefreshed: func(v int64) { refreshedVersion.Store(v) },
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := c.getConfig()
	if cfg == nil || cfg.Version != 3 {
		t.Fatalf("expected cached config version 3, got %+v", cfg)
	}

	// start() only guarantees c.ready has closed (per sdk-conformance's
	// "Readiness resolves before callbacks run" — readiness is not
	// defined to wait for the callback too), and closing ready happens
	// a couple of statements before attemptFetch invokes
	// onConfigRefreshed — so a goroutine scheduled aggressively enough
	// (observed reliably in CI, though not locally) can reach this
	// assertion before that callback actually runs. Poll for it instead
	// of asserting immediately.
	waitFor(t, time.Second, func() bool { return refreshedVersion.Load() == 3 })
}

func TestConfigurationClient_MalformedResponseDoesNotReplaceCache(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)

		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			_, _ = w.Write(testConfigJSON(1))

			return
		}

		_, _ = w.Write([]byte(`{"not": "a valid configuration"}`))
	}))
	defer server.Close()

	errCh := make(chan error, 10)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled:    true,
		refreshInterval:      20 * time.Millisecond,
		onConfigRefreshError: func(err error) { errCh <- err },
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected onConfigRefreshError to be called after the malformed response")
	}

	cfg := c.getConfig()
	if cfg == nil || cfg.Version != 1 {
		t.Fatalf("expected cache to remain at version 1, got %+v", cfg)
	}
}

// TestConfigurationClient_MalformedElementDoesNotReplaceCache exercises
// harden-sdk-runtime task 5.1/5.2's "Validation covers element shape"
// scenario: the top-level containers (flags/rules/rollout arrays) are all
// present and well-formed here, only a single element deep inside is
// malformed (a rollout split's percentage sent as a string, not a
// number). Go's static typing means json.Decode itself rejects this —
// there is no separate "container looked fine, only its element didn't"
// code path to bypass the way there was in the JS clients before task
// 5.1 (see js-sdk's isValidVariation/isValidRule), since a struct field's
// type mismatch fails the whole decode rather than leaving a zero value in
// just that element. This test proves the existing decode-error path
// already satisfies the scenario for this client, rather than that new
// validation code was required here.
func TestConfigurationClient_MalformedElementDoesNotReplaceCache(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)

		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			_, _ = w.Write(testConfigJSON(1))

			return
		}

		// Well-formed containers throughout; only rollout[0].percentage
		// is the wrong JSON type (string instead of number).
		_, _ = w.Write([]byte(`{
			"environment_id": "env_1",
			"version": 2,
			"flags": [{
				"flag_key": "checkout-redesign",
				"enabled": true,
				"default_variation": "off",
				"variations": [{"key": "on", "value": true}, {"key": "off", "value": false}],
				"rules": [{
					"conditions": [],
					"outcome": {"rollout": [{"variation_key": "on", "percentage": "fifty"}]}
				}]
			}]
		}`))
	}))
	defer server.Close()

	errCh := make(chan error, 10)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled:    true,
		refreshInterval:      20 * time.Millisecond,
		onConfigRefreshError: func(err error) { errCh <- err },
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected onConfigRefreshError to be called after the malformed-element response")
	}

	cfg := c.getConfig()
	if cfg == nil || cfg.Version != 1 {
		t.Fatalf("expected cache to remain at version 1, got %+v", cfg)
	}
}

func TestConfigurationClient_RepeatedFailuresKeepLastKnownGood(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)

		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(testConfigJSON(1))

			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	errCh := make(chan error, 10)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled:    true,
		refreshInterval:      20 * time.Millisecond,
		onConfigRefreshError: func(err error) { errCh <- err },
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Backoff after the first failure is the fixed baseBackoff (1s), then
	// doubles; two failures (the first scheduled at refreshIntervalMs,
	// the second at ~1s of backoff) is enough to prove failures keep
	// arriving without ever replacing the cache, without the test itself
	// waiting out several backoff doublings.
	received := 0

	deadline := time.After(5 * time.Second)

	for received < 2 {
		select {
		case <-errCh:
			received++
		case <-deadline:
			t.Fatalf("expected at least 2 refresh errors, got %d", received)
		}
	}

	cfg := c.getConfig()
	if cfg == nil || cfg.Version != 1 {
		t.Fatalf("expected cache to remain at version 1 despite repeated failures, got %+v", cfg)
	}
}

// TestConfigurationClient_HungConnectionIsAbandoned exercises
// harden-sdk-runtime task 4.1: a connection that hangs (the server
// accepts it but never responds — distinct from connection-refused, which
// already fails fast on its own) must be abandoned once requestTimeout
// elapses, and the poll loop must continue on its normal schedule rather
// than stalling on it indefinitely. Before this fix, this client fell
// back to http.DefaultClient (Timeout == 0, no deadline at all) with no
// per-request context deadline either, so attemptFetch — and with it, the
// whole poll loop, since pollLoop only schedules its next attempt after
// the current one returns — would have blocked for as long as the
// connection stayed open. Manually verified: reverting the per-request
// context.WithTimeout made this test itself time out at 2s waiting for
// the first error; reverted back before committing.
func TestConfigurationClient_HungConnectionIsAbandoned(t *testing.T) {
	unblock := make(chan struct{})
	defer close(unblock)

	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		// Accepts the connection but never writes a response until the
		// test itself is done — the genuine hang this test targets.
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	errCh := make(chan error, 10)
	startedAt := time.Now()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled:    true,
		requestTimeout:       50 * time.Millisecond,
		onConfigRefreshError: func(err error) { errCh <- err },
	})
	defer c.close()

	// Long enough that this ctx's own deadline is never what start()
	// actually returns on — the point is to observe the first request's
	// own completion time, not race it against a second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = c.start(ctx) }()

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the first abandoned request to report an error well before 2s")
	}

	elapsed := time.Since(startedAt)

	// Generous upper bound for CI scheduling jitter; still an order of
	// magnitude below what blocking on the hung connection until this
	// test's own 3s ctx (let alone indefinitely) would take.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("expected the abandoned request to be reported well under 500ms (requestTimeout=50ms), took %v", elapsed)
	}

	if got := requestCount.Load(); got < 1 {
		t.Fatalf("expected at least one request attempt to have reached the server, got %d", got)
	}

	// The poll loop's own retry backoff (baseBackoff, 1s, unexported and
	// not configurable via options) schedules the next attempt after the
	// first failure — waiting for a second reported error proves the
	// abandoned connection did not stall the loop, not only that the
	// first request was itself abandoned correctly.
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the poll loop to continue and report a second error within 2s")
	}
}

func TestConfigurationClient_StartReturnsCtxErrIfNeverSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		refreshInterval:   20 * time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := c.start(ctx)
	if err == nil {
		t.Fatal("expected an error when ctx is done before any fetch succeeds")
	}
}

// TestConfigurationClient_CredentialRejectionFailsFastWithoutRetry proves
// task 2.2 for go-sdk: a 401/403 response fails start() immediately (well
// under the poll interval, let alone the ctx deadline) and never causes a
// second request, since retrying a rejected credential can only ever
// reproduce the same rejection. Manually verified load-bearing: with the
// 401/403 branch's condition short-circuited to always-false, this test's
// duration assertion failed (start() instead blocked until the 2s ctx
// deadline) and its request-count assertion failed (multiple requests were
// made); reverted before committing.
func TestConfigurationClient_CredentialRejectionFailsFastWithoutRetry(t *testing.T) {
	var requestCount atomic.Int64

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requestCount.Store(0)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()

			c := newConfigurationClient(server.URL, "svc_bogus.invalid", configurationClientOptions{
				streamingDisabled: true,
				refreshInterval:   20 * time.Millisecond,
			})
			defer c.close()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			start := time.Now()
			err := c.start(ctx)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected an error for a rejected credential")
			}

			if !errors.Is(err, ErrCredentialRejected) {
				t.Fatalf("expected errors.Is(err, ErrCredentialRejected), got: %v", err)
			}

			if elapsed > 200*time.Millisecond {
				t.Fatalf("expected start() to fail fast, took %s (nowhere near the 2s ctx deadline)", elapsed)
			}

			// Give the poll loop a chance to schedule a second attempt, if
			// (wrongly) it still would.
			time.Sleep(100 * time.Millisecond)

			if got := requestCount.Load(); got != 1 {
				t.Fatalf("expected exactly 1 request (no retry after a terminal rejection), got %d", got)
			}
		})
	}
}

func TestConfigurationClient_IsStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testConfigJSON(1))
	}))
	defer server.Close()

	t.Run("true before any successful fetch", func(t *testing.T) {
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{streamingDisabled: true})
		defer c.close()

		if !c.isStale() {
			t.Fatal("expected isStale to be true before any fetch")
		}
	})

	t.Run("false without maxConfigAge, however old the cache", func(t *testing.T) {
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{streamingDisabled: true})
		defer c.close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := c.start(ctx); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		c.lastFetchedAt.Store(time.Now().Add(-24 * time.Hour).UnixNano())

		if c.isStale() {
			t.Fatal("expected isStale to remain false without maxConfigAge set")
		}
	})

	t.Run("respects maxConfigAge when set", func(t *testing.T) {
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{maxConfigAge: 50 * time.Millisecond, streamingDisabled: true})
		defer c.close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := c.start(ctx); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if c.isStale() {
			t.Fatal("expected isStale to be false immediately after a fetch")
		}

		time.Sleep(100 * time.Millisecond)

		if !c.isStale() {
			t.Fatal("expected isStale to be true once maxConfigAge has elapsed")
		}
	})
}

// TestConfigurationClient_ConcurrentReadsDuringRefresh is the race-detector
// test backing openspec/specs/sdk-go/spec.md's "Concurrency-Safe
// Evaluation" requirement: many goroutines reading the cached Configuration
// concurrently with a background refresh swapping it must never race, and
// every reader must observe one complete Configuration value, never a
// partially updated one. Run with `go test -race`.
func TestConfigurationClient_ConcurrentReadsDuringRefresh(t *testing.T) {
	var version atomic.Int64
	version.Store(1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testConfigJSON(version.Add(1)))
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		refreshInterval:   time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stop := make(chan struct{})

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				cfg := c.getConfig()
				if cfg == nil {
					t.Error("expected a non-nil cached config")

					return
				}

				// A read of every field of one snapshot must be internally
				// consistent (same struct, not a torn pointer swap).
				if len(cfg.Flags) != 1 || cfg.Flags[0].FlagKey != "checkout-redesign" {
					t.Errorf("observed a partially-updated Configuration: %+v", cfg)

					return
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestConfigurationClient_PanickingOnConfigRefreshedDoesNotCrash exercises
// harden-sdk-runtime task 3.4: a panicking OnConfigRefreshed must not
// terminate the process. Unlike a JavaScript exception, an unrecovered Go
// panic kills the entire test binary regardless of which goroutine it
// originates in — so the mere fact that this test (and every one after
// it) runs to completion is itself part of the proof; the explicit
// assertions additionally confirm start() still succeeds and the client's
// own state (readiness, cached config) advanced normally despite the
// panic.
func TestConfigurationClient_PanickingOnConfigRefreshedDoesNotCrash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testConfigJSON(5))
	}))
	defer server.Close()

	var callbackRan atomic.Bool

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		onConfigRefreshed: func(int64) {
			callbackRan.Store(true)
			panic("integrator's success callback itself panics")
		},
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !callbackRan.Load() {
		t.Fatal("expected the panicking callback to have been invoked")
	}

	if cfg := c.getConfig(); cfg == nil || cfg.Version != 5 {
		t.Fatalf("expected cached config version 5 despite the panic, got %+v", cfg)
	}
}

// TestConfigurationClient_PanickingOnConfigRefreshErrorDoesNotCrash is the
// same proof for the error-reporting callback (task 3.4), across several
// consecutive failed refresh attempts, not only the first.
func TestConfigurationClient_PanickingOnConfigRefreshErrorDoesNotCrash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var callbackCount atomic.Int64

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		onConfigRefreshError: func(error) {
			callbackCount.Add(1)
			panic("integrator's error callback itself panics")
		},
	})
	defer c.close()

	// The server never returns a valid config, so start() blocks until
	// its own ctx is done; baseBackoff (1s, unexported and not
	// configurable via options, matching @rollfuse/js-sdk's own hardcoded
	// BASE_BACKOFF_MS) is the retry interval after the first, immediate
	// attempt, so this needs to span at least one retry to prove the
	// background poll loop survives more than the very first panic.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	// Expected to time out (the server never returns a valid config); the
	// point is that reaching this line at all, more than once, proves the
	// background poll loop survived a panicking callback on every retry.
	_ = c.start(ctx)

	if got := callbackCount.Load(); got < 2 {
		t.Fatalf("expected the panicking error callback to have been invoked more than once, got %d", got)
	}
}

// TestConfigurationClient_PresentsETagAndAcceptsNotModified exercises task
// 9.1: the client presents its last-seen ETag via If-None-Match, and a
// 304 response is a successful poll (cache retained, not an error, not
// replaced) rather than falling through to the generic error path.
// Manually verified: removing the If-None-Match header, and separately
// removing the 304 special case, each made this test fail for the
// expected reason; restored before committing.
func TestConfigurationClient_PresentsETagAndAcceptsNotModified(t *testing.T) {
	var requestCount atomic.Int32

	var secondRequestIfNoneMatch atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)

		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write(testConfigJSON(1))

			return
		}

		secondRequestIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	errCh := make(chan error, 10)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled:    true,
		refreshInterval:      10 * time.Millisecond,
		onConfigRefreshError: func(err error) { errCh <- err },
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deadline := time.After(2 * time.Second)

	for requestCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("expected a second request within the deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// A 304 is a successful poll: no error reported.
	select {
	case err := <-errCh:
		t.Fatalf("expected no refresh error from a 304 response, got %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if got, _ := secondRequestIfNoneMatch.Load().(string); got != `"v1"` {
		t.Fatalf(`expected If-None-Match: "v1" on the second request, got %q`, got)
	}

	if cfg := c.getConfig(); cfg == nil || cfg.Version != 1 {
		t.Fatalf("expected the cache to remain at version 1, got %+v", cfg)
	}
}

// TestWithJitter exercises task 9.2 directly against the pure function,
// rather than inferring it from wall-clock timing across real goroutines/
// HTTP servers: OS scheduling noise made an earlier version of this test
// (comparing retry-arrival spread across several real clients) pass even
// with jitter disabled, since goroutine/network noise alone already
// exceeded the threshold that version checked. This version cannot false-
// positive that way. Manually verified: pinning withJitter to return base
// unmodified made every one of 200 calls return exactly base, collapsing
// the distinct-value count to 1 and failing both assertions below;
// restored before committing.
func TestWithJitter(t *testing.T) {
	const base = time.Second

	seen := make(map[time.Duration]bool)

	for i := 0; i < 200; i++ {
		delay := withJitter(base)

		if delay < base {
			t.Fatalf("expected withJitter to never return less than base (%v), got %v", base, delay)
		}

		if delay > base+time.Duration(float64(base)*jitterRatio) {
			t.Fatalf("expected withJitter to never exceed base + jitterRatio (%v), got %v", base+time.Duration(float64(base)*jitterRatio), delay)
		}

		seen[delay] = true
	}

	if len(seen) < 2 {
		t.Fatalf("expected withJitter(%v) to return varying delays across 200 calls, got only %d distinct value(s)", base, len(seen))
	}
}

// TestConfigurationClient_HonorsAdvisedPollInterval exercises task 9.3:
// the platform's advised PollIntervalSeconds is used in preference to
// this package's own default when no explicit WithRefreshInterval was
// given. Manually verified: reverting currentRefreshInterval to ignore
// the advised value made this test's second request never arrive within
// its deadline; restored before committing.
func TestConfigurationClient_HonorsAdvisedPollInterval(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)

		cfg := Configuration{
			EnvironmentID:       "env_1",
			Version:             1,
			PollIntervalSeconds: 1, // 1s advised, far below the 30s default
			Flags:               []FlagConfig{},
		}
		body, _ := json.Marshal(cfg)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		// No refreshInterval: falls back to the platform's advised 1s,
		// not this package's own 30s default.
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deadline := time.After(2 * time.Second)

	for requestCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("expected a second poll within ~1s (the advised interval, plus jitter), not the 30s default")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestConfigurationClient_ExplicitRefreshIntervalWinsOverAdvised is the
// control for task 9.3: an explicitly configured refreshInterval still
// wins over the platform's advised interval.
func TestConfigurationClient_ExplicitRefreshIntervalWinsOverAdvised(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)

		cfg := Configuration{EnvironmentID: "env_1", Version: 1, PollIntervalSeconds: 1, Flags: []FlagConfig{}}
		body, _ := json.Marshal(cfg)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		refreshInterval:   500 * time.Millisecond, // explicit — wins over the advised 1s
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if got := requestCount.Load(); got != 1 {
		t.Fatalf("expected still only 1 request 200ms in (below the explicit 500ms interval), got %d", got)
	}

	deadline := time.After(2 * time.Second)

	for requestCount.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("expected a second poll within the explicit ~500ms interval")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
