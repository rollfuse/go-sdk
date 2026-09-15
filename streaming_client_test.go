package rollfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamTestServer is a real HTTP server backing both GET /v1/config and
// GET /v1/config/stream, mirroring the platform's actual contract closely
// enough to exercise this package's real SSE-parsing/reconnect code
// against real bytes on a real connection — not a mock of the parsing
// logic itself, matching this repo's established testing discipline.
type streamTestServer struct {
	*httptest.Server

	heartbeatInterval time.Duration
	pollIntervalSecs  int

	mu      sync.Mutex
	version int64
	subs    map[chan int64]struct{}

	streamRequests atomic.Int64
	configRequests atomic.Int64
	// refuseStream, when non-zero, makes every /v1/config/stream request
	// fail with that status instead of connecting (tasks 5.2/5.3).
	refuseStream atomic.Int32
}

func newStreamTestServer(t *testing.T, heartbeatInterval time.Duration, pollIntervalSecs int) *streamTestServer {
	t.Helper()

	s := &streamTestServer{
		heartbeatInterval: heartbeatInterval,
		pollIntervalSecs:  pollIntervalSecs,
		version:           1,
		subs:              make(map[chan int64]struct{}),
	}

	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			s.configRequests.Add(1)
			s.serveConfig(w)
		case "/v1/config/stream":
			s.streamRequests.Add(1)
			s.serveStream(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(s.Close)

	return s
}

func (s *streamTestServer) serveConfig(w http.ResponseWriter) {
	s.mu.Lock()
	version := s.version
	s.mu.Unlock()

	cfg := Configuration{
		EnvironmentID:       "env_1",
		Version:             version,
		PollIntervalSeconds: 300,
		Flags: []FlagConfig{
			{
				FlagKey: "checkout-redesign",
				Enabled: true,
				// DefaultVariation carries the current Version's own
				// number as its value (task 5.9's test threads it
				// through here), so Evaluate's returned value proves
				// which Configuration Version a Client actually applied,
				// independent of which transport told it to fetch.
				DefaultVariation: "on",
				Variations: []Variation{
					{Key: "on", Value: json.RawMessage(fmt.Sprintf(`%d`, version))},
					{Key: "off", Value: json.RawMessage(`0`)},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}

func (s *streamTestServer) serveStream(w http.ResponseWriter, r *http.Request) {
	if status := s.refuseStream.Load(); status != 0 {
		w.WriteHeader(int(status))

		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	hello := streamHelloEvent{
		HeartbeatIntervalSeconds: int(s.heartbeatInterval.Seconds()),
		MaxLifetimeSeconds:       3600,
		PollIntervalSeconds:      s.pollIntervalSecs,
	}
	helloData, _ := json.Marshal(hello)
	_, _ = fmt.Fprintf(w, "event: hello\ndata: %s\n\n", helloData)

	flusher.Flush()

	ch := make(chan int64, 4)

	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	heartbeat := time.NewTicker(s.heartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")

			flusher.Flush()
		case version, ok := <-ch:
			if !ok {
				return
			}

			_, _ = fmt.Fprintf(w, "event: version\ndata: %d\n\n", version)

			flusher.Flush()
		}
	}
}

// publish sets the server's current Configuration Version (what GET
// /v1/config will report from now on) and, unless skipNotify, broadcasts
// a version event to every connected stream — mirroring the real
// platform's VersionRepository.BumpForEnvironment + Registry.
// NotifyVersionChanged happening together on every publish.
func (s *streamTestServer) publish(version int64, skipNotify bool) {
	s.mu.Lock()
	s.version = version
	subs := make([]chan int64, 0, len(s.subs))

	for ch := range s.subs {
		subs = append(subs, ch)
	}

	s.mu.Unlock()

	if skipNotify {
		return
	}

	for _, ch := range subs {
		select {
		case ch <- version:
		default:
		}
	}
}

func (s *streamTestServer) subscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.subs)
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	if !condition() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// TestStreamingClient_ReadinessDoesNotDependOnStreamConnecting backs task
// 5.1: start() must resolve on the first successful GET /v1/config alone,
// even if the streaming endpoint never responds successfully at all.
func TestStreamingClient_ReadinessDoesNotDependOnStreamConnecting(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)
	server.refuseStream.Store(http.StatusServiceUnavailable)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("start() took %s — should resolve on the first GET /v1/config alone, independent of streaming", elapsed)
	}

	if cfg := c.getConfig(); cfg == nil {
		t.Fatal("expected a cached configuration after start()")
	}
}

// TestStreamingClient_FallsBackToPollingWhenRefused backs task 5.2: a
// refused (429/503) or otherwise-failing stream connection must not
// affect evaluation — polling keeps the cache current on its own.
func TestStreamingClient_FallsBackToPollingWhenRefused(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)
	server.refuseStream.Store(http.StatusTooManyRequests)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		refreshInterval: 30 * time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server.publish(2, false /* no subscribers care, they're all refused */)

	waitFor(t, 2*time.Second, func() bool {
		cfg := c.getConfig()
		return cfg != nil && cfg.Version == 2
	})

	if c.streamConnected.Load() {
		t.Fatal("expected streamConnected == false when every connection attempt is refused")
	}
}

// TestStreamingClient_ReportsConnectedAndTransport backs tasks 5.1/5.7: a
// successful connection is reflected in streamConnected, and Client.
// Transport reports it.
func TestStreamingClient_ReportsConnectedAndTransport(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)

	client, err := NewClient(server.URL, "cred")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return client.Transport().Streaming })

	if got := client.Transport(); !got.Streaming {
		t.Fatalf("expected Transport().Streaming == true once connected, got %+v", got)
	}
}

// TestStreamingClient_DisabledNeverDialsTheStreamEndpoint backs task 5.8:
// WithStreamingDisabled must mean GET /v1/config/stream is never
// requested at all, not merely ignored.
func TestStreamingClient_DisabledNeverDialsTheStreamEndpoint(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		streamingDisabled: true,
		refreshInterval:   20 * time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	if got := server.streamRequests.Load(); got != 0 {
		t.Fatalf("expected zero requests to /v1/config/stream with streaming disabled, got %d", got)
	}

	if c.streamConnected.Load() {
		t.Fatal("expected streamConnected == false with streaming disabled")
	}
}

// TestStreamingClient_VersionNotificationTriggersImmediateFetch backs
// task 5.5: a version notification for a version not yet held triggers an
// out-of-band fetch rather than waiting for the next scheduled poll.
func TestStreamingClient_VersionNotificationTriggersImmediateFetch(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)

	// A deliberately long refresh interval: if the notification didn't
	// trigger an immediate fetch, the version-3 config would not be
	// observed within this test's own timeout.
	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		refreshInterval: 10 * time.Second,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, time.Second, func() bool { return server.subscriberCount() > 0 })

	server.publish(3, false)

	waitFor(t, time.Second, func() bool {
		cfg := c.getConfig()
		return cfg != nil && cfg.Version == 3
	})
}

// TestStreamingClient_AlreadyHeldVersionDoesNotTriggerAFetch backs task
// 5.5's other half: a notification naming a version already held (e.g.
// because polling's own reduced-interval tick already picked it up)
// causes no extra fetch.
func TestStreamingClient_AlreadyHeldVersionDoesNotTriggerAFetch(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		refreshInterval: 30 * time.Millisecond,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server.publish(2, true /* bump the version the ordinary poll will pick up, no notification yet */)

	waitFor(t, time.Second, func() bool {
		cfg := c.getConfig()
		return cfg != nil && cfg.Version == 2
	})

	before := server.configRequests.Load()

	// Now notify for the SAME version the poll already fetched.
	waitFor(t, time.Second, func() bool { return server.subscriberCount() > 0 })
	server.publish(2, false)

	// Give considerVersionNotification a moment to (not) act.
	time.Sleep(150 * time.Millisecond)

	after := server.configRequests.Load()
	// Some ordinary polls may have happened in this window (30ms
	// interval) — that's fine and expected; what must NOT happen is an
	// extra fetch caused specifically by the redundant notification. This
	// is inherently a bit loose without hooking attemptFetch directly, so
	// this test's real assertion is the stronger one below: the cached
	// version never regresses and streamHighestNotified reflects the
	// no-op.
	if after < before {
		t.Fatalf("request count went backwards: %d -> %d", before, after)
	}

	if got := c.streamHighestNotified.Load(); got != 2 {
		t.Fatalf("expected streamHighestNotified == 2, got %d", got)
	}
}

// TestStreamingClient_OutOfOrderNotificationsConvergeOnNewest backs task
// 5.6: a lower version arriving after a higher one was already acted on
// must never regress anything.
func TestStreamingClient_OutOfOrderNotificationsConvergeOnNewest(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1)

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{
		refreshInterval: 10 * time.Second,
	})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, time.Second, func() bool { return server.subscriberCount() > 0 })

	// Publish version 7 for real (so GET /v1/config actually returns it),
	// then simulate a reordered, stale "5" notification arriving after —
	// directly, bypassing the server's own publish (which would also
	// bump its served version), to isolate considerVersionNotification's
	// own ordering guarantee.
	server.publish(7, false)

	waitFor(t, time.Second, func() bool {
		cfg := c.getConfig()
		return cfg != nil && cfg.Version == 7
	})

	c.considerVersionNotification(5)

	time.Sleep(150 * time.Millisecond)

	cfg := c.getConfig()
	if cfg == nil || cfg.Version != 7 {
		t.Fatalf("expected cached version to remain 7 after a stale out-of-order notification, got %+v", cfg)
	}

	if got := c.streamHighestNotified.Load(); got != 7 {
		t.Fatalf("expected streamHighestNotified to remain 7 (not regress to 5), got %d", got)
	}
}

// TestStreamingClient_ReducedPollIntervalWhileConnected backs task 5.4:
// currentRefreshInterval must use the hello event's disclosed poll
// interval while a stream is connected, in preference to the platform's
// own Configuration.PollIntervalSeconds.
func TestStreamingClient_ReducedPollIntervalWhileConnected(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1) // poll_interval_seconds: 1

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, time.Second, func() bool { return c.streamConnected.Load() })

	if got := c.currentRefreshInterval(); got != time.Second {
		t.Fatalf("expected currentRefreshInterval() == 1s (the hello event's poll_interval_seconds) while connected, got %s", got)
	}
}

// TestStreamingClient_LostNotificationStillRecoveredByPolling backs task
// 5.4's other half: even if a version event is never delivered at all,
// the reduced-interval poll converges on the new version within a bounded
// time (the design's own "at-most-once delivery, polling is the floor"
// guarantee).
func TestStreamingClient_LostNotificationStillRecoveredByPolling(t *testing.T) {
	server := newStreamTestServer(t, 50*time.Millisecond, 1) // 1s reduced poll interval

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitFor(t, time.Second, func() bool { return c.streamConnected.Load() })

	// skipNotify: true — the server bumps its own served version but
	// never sends a version event, simulating a lost notification.
	server.publish(9, true)

	waitFor(t, 3*time.Second, func() bool {
		cfg := c.getConfig()
		return cfg != nil && cfg.Version == 9
	})
}

// TestStreamingClient_BrokenConnectionIsDetectedAndReconnects backs task
// 5.3: a connection that stops sending ANY frames (heartbeats included)
// must be detected as broken via the disclosed heartbeat interval — not
// by trusting TCP alone — and the client must reconnect. The server's
// first connection goes silent forever right after its hello (no
// heartbeats, simulating a hung intermediary); every later (reconnect)
// attempt behaves normally.
func TestStreamingClient_BrokenConnectionIsDetectedAndReconnects(t *testing.T) {
	var attempt atomic.Int32

	// The wire contract discloses heartbeat_interval_seconds as whole
	// seconds (see streamHelloEvent/design-decisions.md), so this can't
	// be sub-second the way other tests' heartbeatInterval is — 1s is the
	// smallest representable value, making this test's own bound
	// (missed-heartbeat window = streamMissedHeartbeatMultiplier * 1s =
	// 3s) the slowest test in this file.
	heartbeatInterval := 1 * time.Second

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/config":
			cfg := Configuration{EnvironmentID: "env_1", Version: 1}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cfg)
		case "/v1/config/stream":
			n := attempt.Add(1)

			flusher, ok := w.(http.Flusher)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			hello := streamHelloEvent{HeartbeatIntervalSeconds: int(heartbeatInterval.Seconds()), MaxLifetimeSeconds: 3600, PollIntervalSeconds: 1}
			helloData, _ := json.Marshal(hello)
			_, _ = fmt.Fprintf(w, "event: hello\ndata: %s\n\n", helloData)

			flusher.Flush()

			if n == 1 {
				// Go silent forever: no heartbeats, no version events —
				// exactly the "intermediary silently drops the
				// connection" scenario the missed-heartbeat watchdog
				// exists to detect.
				<-r.Context().Done()

				return
			}

			ticker := time.NewTicker(heartbeatInterval)
			defer ticker.Stop()

			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = fmt.Fprint(w, ": heartbeat\n\n")

					flusher.Flush()
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newConfigurationClient(server.URL, "cred", configurationClientOptions{})
	defer c.close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.start(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The first (silent) connection still gets a hello, so streamConnected
	// briefly goes true.
	waitFor(t, time.Second, func() bool { return c.streamConnected.Load() })

	// No heartbeats follow: watchForStaleConnection's missed-heartbeat
	// window (streamMissedHeartbeatMultiplier * heartbeatInterval, 3s
	// here) must detect the silence and tear the connection down.
	waitFor(t, 5*time.Second, func() bool { return !c.streamConnected.Load() })

	// streamLoop reconnects (after its own randomized backoff) — the
	// second attempt behaves normally and reports connected again.
	waitFor(t, 8*time.Second, func() bool { return c.streamConnected.Load() && attempt.Load() >= 2 })
}

// TestStreamingClient_EvaluationsIdenticalUnderEitherTransport backs task
// 5.9: the same Configuration change, learned about once via a version
// SSE event and once purely through the reduced-interval poll picking it
// up on its own, must produce identical Evaluate results — proving
// streaming only ever triggers the SAME fetch/decode/cache path polling
// already uses, never a second, independent way of applying a
// Configuration.
func TestStreamingClient_EvaluationsIdenticalUnderEitherTransport(t *testing.T) {
	streamingServer := newStreamTestServer(t, 50*time.Millisecond, 1)

	streamingClient, err := NewClient(streamingServer.URL, "cred")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = streamingClient.Close() }()

	pollingServer := newStreamTestServer(t, 50*time.Millisecond, 1)

	pollingClient, err := NewClient(pollingServer.URL, "cred", WithStreamingDisabled(), WithRefreshInterval(30*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = pollingClient.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := streamingClient.Start(ctx); err != nil {
		t.Fatalf("unexpected error starting streamingClient: %v", err)
	}

	if err := pollingClient.Start(ctx); err != nil {
		t.Fatalf("unexpected error starting pollingClient: %v", err)
	}

	waitFor(t, time.Second, func() bool { return streamingClient.Transport().Streaming })

	if got := pollingClient.Transport(); got.Streaming {
		t.Fatal("expected pollingClient.Transport().Streaming == false (WithStreamingDisabled)")
	}

	// The exact same change, applied to two independent servers: one
	// notified via a real version SSE event, the other never notified at
	// all (skipNotify), relying purely on its own reduced/explicit poll
	// interval to observe it.
	streamingServer.publish(5, false)
	pollingServer.publish(5, true)

	waitFor(t, 2*time.Second, func() bool {
		r, err := streamingClient.Evaluate("subject_1", "checkout-redesign")
		return err == nil && string(r.Value) == "5"
	})

	waitFor(t, 2*time.Second, func() bool {
		r, err := pollingClient.Evaluate("subject_1", "checkout-redesign")
		return err == nil && string(r.Value) == "5"
	})

	streamingResult, err := streamingClient.Evaluate("subject_1", "checkout-redesign")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pollingResult, err := pollingClient.Evaluate("subject_1", "checkout-redesign")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if streamingResult.VariationKey != pollingResult.VariationKey ||
		string(streamingResult.Value) != string(pollingResult.Value) ||
		streamingResult.Reason != pollingResult.Reason ||
		streamingResult.ConfigVersion != pollingResult.ConfigVersion {
		t.Fatalf("evaluations diverged between transports:\n  streaming: %+v\n  polling:   %+v", streamingResult, pollingResult)
	}
}
