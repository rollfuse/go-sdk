package rollfuse_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rollfuse "github.com/rollfuse/go-sdk"
)

func testConfigurationServer(t *testing.T, flag rollfuse.FlagConfig, version int64) *httptest.Server {
	t.Helper()

	cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: version, Flags: []rollfuse.FlagConfig{flag}}

	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("unexpected error marshaling test configuration: %v", err)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "/v1/exposure-events":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accepted":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func matchedFlag() rollfuse.FlagConfig {
	return rollfuse.FlagConfig{
		FlagKey:          "checkout-redesign",
		Enabled:          true,
		DefaultVariation: "off",
		Variations: []rollfuse.Variation{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		},
		Rules: []rollfuse.Rule{
			{
				Conditions: []rollfuse.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    rollfuse.Outcome{VariationKey: "on"},
			},
		},
	}
}

func TestClient_ExplicitCredentialConfiguration(t *testing.T) {
	_, err := rollfuse.NewClient("http://api.test", "")
	if !errors.Is(err, rollfuse.ErrCredentialRequired) {
		t.Fatalf("expected ErrCredentialRequired, got %v", err)
	}
}

func TestClient_SafeFallback_NoCacheYetWithFallback(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Start not called: no Configuration cached yet.
	result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithFallback("fallback-value"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var value string
	if err := json.Unmarshal(result.Value, &value); err != nil {
		t.Fatalf("unexpected error unmarshaling fallback value: %v", err)
	}

	if value != "fallback-value" || result.Reason != rollfuse.ReasonDefaultFallback || result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClient_SafeFallback_NoCacheYetNoFallback(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Evaluate("user_1", "checkout-redesign")
	if !errors.Is(err, rollfuse.ErrConfigNotReady) {
		t.Fatalf("expected ErrConfigNotReady, got %v", err)
	}
}

func TestClient_EvaluateAll_NoCacheYet(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.EvaluateAll("user_1")
	if !errors.Is(err, rollfuse.ErrConfigNotReady) {
		t.Fatalf("expected ErrConfigNotReady, got %v", err)
	}
}

func TestClient_UnknownFlagKey(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = client.Evaluate("user_1", "does-not-exist")
	if !errors.Is(err, rollfuse.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound, got %v", err)
	}
}

// TestClient_NonEvaluableFlag_ServesFallback is expand-targeting-model
// task 3.3/3.5's own verification: a flag the platform marks
// non-evaluable (a construct newer than this client understands) MUST
// serve the caller's fallback, never a value derived from a definition
// this client was never shown.
func TestClient_NonEvaluableFlag_ServesFallback(t *testing.T) {
	nonEvaluable := rollfuse.FlagConfig{FlagKey: "future-flag", NonEvaluable: true}

	server := testConfigurationServer(t, nonEvaluable, 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = client.Evaluate("user_1", "future-flag")
	if !errors.Is(err, rollfuse.ErrFlagNotEvaluable) {
		t.Fatalf("expected ErrFlagNotEvaluable with no fallback supplied, got %v", err)
	}

	result, err := client.Evaluate("user_1", "future-flag", rollfuse.WithFallback("caller-fallback"))
	if err != nil {
		t.Fatalf("unexpected error with fallback supplied: %v", err)
	}

	var value string
	if err := json.Unmarshal(result.Value, &value); err != nil || value != "caller-fallback" {
		t.Fatalf("expected caller-supplied fallback value, got %s (err=%v)", result.Value, err)
	}

	if result.Reason != rollfuse.ReasonDefaultFallback {
		t.Fatalf("expected ReasonDefaultFallback, got %q", result.Reason)
	}

	all, err := client.EvaluateAll("user_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(all) != 0 {
		t.Fatalf("expected EvaluateAll to omit the non-evaluable flag entirely, got %+v", all)
	}
}

func TestClient_FailureIsolation_KeepsServingAfterRefreshFailure(t *testing.T) {
	var requestCount atomic.Int32

	cfg := rollfuse.Configuration{
		EnvironmentID: "env_1",
		Version:       1,
		Flags:         []rollfuse.FlagConfig{matchedFlag()},
	}
	body, _ := json.Marshal(cfg)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if requestCount.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)

			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled(), rollfuse.WithRefreshInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(100 * time.Millisecond) // let at least one refresh fail

	result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.VariationKey != "on" || result.Reason != rollfuse.ReasonRuleMatch {
		t.Fatalf("unexpected result after refresh failure: %+v", result)
	}
}

func TestClient_Evaluate_DoesNotPerformNetworkRequest(t *testing.T) {
	var configRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/config" {
			configRequests.Add(1)
		}

		cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: 1, Flags: []rollfuse.FlagConfig{matchedFlag()}}

		body, _ := json.Marshal(cfg)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	before := configRequests.Load()

	if _, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := configRequests.Load(); got != before {
		t.Fatalf("expected no additional /v1/config requests from Evaluate, before=%d after=%d", before, got)
	}
}

func TestClient_ExposureReporting_EnqueuesAndSubmitsRuleMatch(t *testing.T) {
	var exposureRequests int32

	exposureCh := make(chan []byte, 10)

	cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: 5, Flags: []rollfuse.FlagConfig{matchedFlag()}}
	cfgBody, _ := json.Marshal(cfg)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cfgBody)
		case "/v1/exposure-events":
			atomic.AddInt32(&exposureRequests, 1)

			buf := make([]byte, r.ContentLength)

			_, _ = r.Body.Read(buf)
			exposureCh <- buf

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accepted":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled(), rollfuse.WithExposureBatchSize(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.TrackExposure {
		t.Fatalf("expected TrackExposure true, got %+v", result)
	}

	select {
	case raw := <-exposureCh:
		var payload struct {
			Events []map[string]any `json:"events"`
		}

		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unexpected error unmarshaling submitted batch: %v", err)
		}

		if len(payload.Events) != 1 {
			t.Fatalf("expected exactly 1 submitted event, got %d", len(payload.Events))
		}

		event := payload.Events[0]
		if event["flag_key"] != "checkout-redesign" || event["subject_key"] != "user_1" || event["variation_key"] != "on" || event["reason"] != "rule_match" {
			t.Fatalf("unexpected submitted event: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an exposure batch to be submitted")
	}
}

func TestClient_ExposureReporting_DefaultResultNeverEnqueues(t *testing.T) {
	var exposureRequests atomic.Int32

	flag := rollfuse.FlagConfig{
		FlagKey:          "always-off",
		Enabled:          false,
		DefaultVariation: "off",
		Variations:       []rollfuse.Variation{{Key: "off", Value: json.RawMessage(`false`)}},
	}

	cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: 1, Flags: []rollfuse.FlagConfig{flag}}
	cfgBody, _ := json.Marshal(cfg)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cfgBody)
		case strings.Contains(r.URL.Path, "exposure-events"):
			exposureRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accepted":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled(), rollfuse.WithExposureBatchSize(1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := client.Evaluate("user_1", "always-off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.TrackExposure {
		t.Fatalf("expected TrackExposure false for a disabled flag, got %+v", result)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := exposureRequests.Load(); got != 0 {
		t.Fatalf("expected no exposure-events request, got %d", got)
	}
}

func TestClient_ExposureReporting_FullQueueDropsAndReports(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	var dropped atomic.Int32

	client, err := rollfuse.NewClient(
		server.URL, "cred",
		rollfuse.WithStreamingDisabled(),
		rollfuse.WithExposureQueueCapacity(1),
		rollfuse.WithExposureBatchSize(1_000_000), // never auto-flush during this test
		rollfuse.WithOnExposureDropped(func(n int) { dropped.Add(int32(n)) }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	attrs := rollfuse.WithAttributes(map[string]string{"plan": "enterprise"})

	if _, err := client.Evaluate("user_1", "checkout-redesign", attrs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := client.Evaluate("user_2", "checkout-redesign", attrs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Drops are now aggregated and reported once per flush cycle (task
	// 7.5), rather than synchronously inside Evaluate/enqueue.
	if got := dropped.Load(); got != 0 {
		t.Fatalf("expected no drops reported yet, got %d", got)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := dropped.Load(); got != 1 {
		t.Fatalf("expected exactly 1 dropped exposure, got %d", got)
	}
}

func TestClient_ExposureReporting_SubmitFailureDoesNotAffectPastResult(t *testing.T) {
	cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: 1, Flags: []rollfuse.FlagConfig{matchedFlag()}}
	cfgBody, _ := json.Marshal(cfg)

	submitErrCh := make(chan error, 5)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cfgBody)
		case "/v1/exposure-events":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(
		server.URL, "cred",
		rollfuse.WithStreamingDisabled(),
		rollfuse.WithExposureBatchSize(1),
		rollfuse.WithOnExposureSubmitError(func(err error) { submitErrCh <- err }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-submitErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected onExposureSubmitError to be called")
	}

	// The result already returned synchronously by Evaluate is unaffected
	// by the later, asynchronous submission failure.
	if result.VariationKey != "on" || result.Reason != rollfuse.ReasonRuleMatch {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestClient_ConcurrentEvaluate is the race-detector test backing
// openspec/specs/sdk-go/spec.md's "Concurrency-Safe Evaluation"
// requirement at the public Client level (configuration_client_test.go
// covers it at the configurationClient level). Run with `go test -race`.
func TestClient_ConcurrentEvaluate(t *testing.T) {
	server := testConfigurationServer(t, matchedFlag(), 1)
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled(), rollfuse.WithRefreshInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup

	stop := make(chan struct{})

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				if _, err := client.Evaluate("user", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"})); err != nil {
					t.Errorf("unexpected error: %v", err)

					return
				}
			}
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestClient_Stop_ResumesOnRestart exercises harden-sdk-runtime task 8.3:
// a Client that was stopped resumes polling and reporting once started
// again, rather than remaining permanently inert. Manually verified: this
// test failed before the fix (Stop() didn't exist, and start()'s guard
// was a permanent one-time flag) — reverting configuration_client.go's
// restart-safe start()/stop() back to the original startOnce-based
// version made the resumed poll never happen, hanging this test's second
// Start() call forever; restored before committing.
func TestClient_Stop_ResumesOnRestart(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		n := requestCount.Add(1)

		flag := matchedFlag()
		if n >= 2 {
			flag.Rules = nil // second+ fetch: the enterprise rule no longer matches
		}

		cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: int64(n), Flags: []rollfuse.FlagConfig{flag}}
		body, _ := json.Marshal(cfg)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithStreamingDisabled(), rollfuse.WithRefreshInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.VariationKey != "on" {
		t.Fatalf("expected the initial fetch to match the enterprise rule, got %+v", result)
	}

	client.Stop()

	countAtStop := requestCount.Load()

	// While stopped, no further refresh happens even after several
	// refresh intervals elapse.
	time.Sleep(100 * time.Millisecond)

	if got := requestCount.Load(); got != countAtStop {
		t.Fatalf("expected no further requests while stopped, went from %d to %d", countAtStop, got)
	}

	// Restarted: resumes polling rather than remaining permanently inert.
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer restartCancel()

	if err := client.Start(restartCtx); err != nil {
		t.Fatalf("unexpected error restarting: %v", err)
	}

	deadline := time.After(2 * time.Second)

	for {
		result, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if result.VariationKey == "off" {
			break // a post-restart fetch landed and changed the served variation
		}

		select {
		case <-deadline:
			t.Fatal("expected polling to resume and eventually serve the post-restart configuration")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := requestCount.Load(); got <= countAtStop {
		t.Fatalf("expected more requests after restart than were made before stop (%d), got %d", countAtStop, got)
	}
}

// TestClient_Close_BoundedByCloseTimeout exercises task 8.2: Close()
// returns once flushed or once WithCloseTimeout elapses, whichever comes
// first, rather than blocking indefinitely on a hung flush. Manually
// verified: reverting Close() to a synchronous, unbounded call (no
// goroutine/select/time.After) made this test itself hang past its own
// deadline; restored before committing.
func TestClient_Close_BoundedByCloseTimeout(t *testing.T) {
	hang := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: 1, Flags: []rollfuse.FlagConfig{matchedFlag()}}
			body, _ := json.Marshal(cfg)

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "/v1/exposure-events":
			<-hang // never responds, until the test itself releases it on cleanup
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// Declared in this order so defers run close(hang) before
	// server.Close(): the server can't finish closing while a handler is
	// still blocked inside <-hang, and Close() itself only bounds the
	// Client's own wait, not this test server's still-pending connection.
	defer server.Close()
	defer close(hang)

	client, err := rollfuse.NewClient(
		server.URL, "cred",
		rollfuse.WithStreamingDisabled(),
		rollfuse.WithCloseTimeout(100*time.Millisecond),
		rollfuse.WithExposureBatchSize(1_000_000), // never auto-flush before Close() itself triggers it
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := client.Evaluate("user_1", "checkout-redesign", rollfuse.WithAttributes(map[string]string{"plan": "enterprise"})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	startedAt := time.Now()

	if err := client.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	elapsed := time.Since(startedAt)

	if elapsed < 90*time.Millisecond {
		t.Fatalf("expected Close() to wait out roughly the close timeout, returned after only %v", elapsed)
	}

	if elapsed > 1*time.Second {
		t.Fatalf("expected Close() to be bounded by closeTimeout, took %v", elapsed)
	}
}

// TestClient_Subscribe_NotifiesOnConfigChange exercises harden-sdk-runtime
// task 10.1's underlying primitive: Subscribe lets a caller that didn't
// construct the Client (an OpenFeature provider wrapping an
// already-built one, per proposal.md's "the Go OpenFeature provider")
// observe Configuration refreshes without needing its own
// WithOnConfigRefreshed at construction time. Also verifies the
// integrator's own WithOnConfigRefreshed (if any) still fires alongside
// subscribers, and that Unsubscribe stops further notifications.
func TestClient_Subscribe_NotifiesOnConfigChange(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		n := requestCount.Add(1)
		cfg := rollfuse.Configuration{EnvironmentID: "env_1", Version: int64(n), Flags: []rollfuse.FlagConfig{matchedFlag()}}
		body, _ := json.Marshal(cfg)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	var ownCallbackCount atomic.Int32

	client, err := rollfuse.NewClient(
		server.URL, "cred",
		rollfuse.WithStreamingDisabled(),
		rollfuse.WithRefreshInterval(20*time.Millisecond),
		rollfuse.WithOnConfigRefreshed(func(int64) { ownCallbackCount.Add(1) }),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	var subscriberCount atomic.Int32

	unsubscribe := client.Subscribe(func() { subscriberCount.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deadline := time.After(2 * time.Second)

	for subscriberCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected at least 3 subscriber notifications, got %d", subscriberCount.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	if got := ownCallbackCount.Load(); got == 0 {
		t.Fatal("expected the integrator's own WithOnConfigRefreshed to still fire alongside subscribers")
	}

	unsubscribe()

	countAtUnsubscribe := subscriberCount.Load()

	time.Sleep(100 * time.Millisecond)

	if got := subscriberCount.Load(); got != countAtUnsubscribe {
		t.Fatalf("expected no further notifications after unsubscribe, went from %d to %d", countAtUnsubscribe, got)
	}
}
