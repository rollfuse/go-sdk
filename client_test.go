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

	client, err := rollfuse.NewClient(server.URL, "cred")
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

	client, err := rollfuse.NewClient(server.URL, "cred")
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

	client, err := rollfuse.NewClient(server.URL, "cred")
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

	client, err := rollfuse.NewClient(server.URL, "cred")
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

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithRefreshInterval(20*time.Millisecond))
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

	client, err := rollfuse.NewClient(server.URL, "cred")
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

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithExposureBatchSize(1))
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

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithExposureBatchSize(1))
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

	client, err := rollfuse.NewClient(server.URL, "cred", rollfuse.WithRefreshInterval(5*time.Millisecond))
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
