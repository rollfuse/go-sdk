package rollfuse

import (
	"context"
	"encoding/json"
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

	if got := refreshedVersion.Load(); got != 3 {
		t.Fatalf("expected onConfigRefreshed(3), got %d", got)
	}
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

func TestConfigurationClient_StartReturnsCtxErrIfNeverSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		refreshInterval: 20 * time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := c.start(ctx)
	if err == nil {
		t.Fatal("expected an error when ctx is done before any fetch succeeds")
	}
}

func TestConfigurationClient_IsStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testConfigJSON(1))
	}))
	defer server.Close()

	t.Run("true before any successful fetch", func(t *testing.T) {
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
		defer c.close()

		if !c.isStale() {
			t.Fatal("expected isStale to be true before any fetch")
		}
	})

	t.Run("false without maxConfigAge, however old the cache", func(t *testing.T) {
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
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
		c := newConfigurationClient(server.URL, "cred", configurationClientOptions{maxConfigAge: 50 * time.Millisecond})
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
		refreshInterval: time.Millisecond,
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
