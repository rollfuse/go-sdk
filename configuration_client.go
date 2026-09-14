package rollfuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var errEmptyEnvironmentID = errors.New("environment_id is empty")

const (
	defaultRefreshInterval = 30 * time.Second
	baseBackoff            = 1 * time.Second
	maxBackoff             = 30 * time.Second
	// defaultRequestTimeout bounds a single GET /v1/config request,
	// matching exposure_queue.go's own submitTimeout — see
	// requestTimeout's own doc comment.
	defaultRequestTimeout = 10 * time.Second
)

// configurationClientOptions configures a configurationClient. Populated by
// Client's own functional options (client.go); unexported since this type
// is an internal implementation detail, not part of the public API.
type configurationClientOptions struct {
	refreshInterval      time.Duration
	maxConfigAge         time.Duration
	requestTimeout       time.Duration
	httpClient           *http.Client
	onConfigRefreshed    func(version int64)
	onConfigRefreshError func(err error)
}

// configurationClient fetches, caches and background-refreshes a
// Credential-scoped Configuration from GET /v1/config, per design.md
// decisions 3 and 6: the cached Configuration lives behind
// atomic.Pointer[Configuration] for lock-free, concurrency-safe reads
// (openspec/specs/sdk-go/spec.md's "Concurrency-Safe Evaluation"
// requirement); start blocks the caller until the first successful fetch
// or its ctx is done, while the background refresh loop runs on an
// internal lifecycle context independent of that ctx. stop halts that
// loop; start after stop resumes it (task 8.3) rather than leaving the
// client permanently inert.
type configurationClient struct {
	baseURL         string
	credential      string
	refreshInterval time.Duration
	maxConfigAge    time.Duration
	// requestTimeout bounds a single GET /v1/config request via a
	// context.WithTimeout derived from the poll loop's current lifecycle
	// context, per sdk-conformance's "Every Network Operation Carries A
	// Deadline" requirement: the default http.Client this package falls
	// back to (http.DefaultClient) has Timeout == 0, no deadline at all,
	// so a hung connection would otherwise stall attemptFetch — and with
	// it, the entire poll loop, since pollLoop only schedules its next
	// attempt after the current one returns — indefinitely.
	requestTimeout       time.Duration
	httpClient           *http.Client
	onConfigRefreshed    func(version int64)
	onConfigRefreshError func(err error)

	config        atomic.Pointer[Configuration]
	lastFetchedAt atomic.Int64 // UnixNano; 0 means never fetched
	backoff       time.Duration

	// lifecycleMu protects running/lifecycleCtx/lifecycleCancel across
	// start()/stop() cycles.
	lifecycleMu     sync.Mutex
	running         bool
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	wg              sync.WaitGroup

	ready     chan struct{}
	readyOnce sync.Once
}

func newConfigurationClient(baseURL, credential string, opts configurationClientOptions) *configurationClient {
	refreshInterval := opts.refreshInterval
	if refreshInterval <= 0 {
		refreshInterval = defaultRefreshInterval
	}

	requestTimeout := opts.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}

	httpClient := opts.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &configurationClient{
		baseURL:              baseURL,
		credential:           credential,
		refreshInterval:      refreshInterval,
		maxConfigAge:         opts.maxConfigAge,
		requestTimeout:       requestTimeout,
		httpClient:           httpClient,
		onConfigRefreshed:    opts.onConfigRefreshed,
		onConfigRefreshError: opts.onConfigRefreshError,
		backoff:              baseBackoff,
		ready:                make(chan struct{}),
	}
}

// start begins the background poll loop and blocks until the first fetch
// succeeds, ctx is done, or the client's own lifecycle ends. Safe to call
// again after stop() (task 8.3): resumes polling rather than remaining
// permanently inert. A plain repeated call while already running returns
// the same in-flight/already-settled readiness.
func (c *configurationClient) start(ctx context.Context) error {
	c.lifecycleMu.Lock()

	if !c.running {
		lifecycleCtx, cancel := context.WithCancel(context.Background())
		c.lifecycleCtx = lifecycleCtx
		c.lifecycleCancel = cancel
		c.running = true

		c.wg.Add(1)

		go c.pollLoop(lifecycleCtx)
	}

	lifecycleCtx := c.lifecycleCtx

	c.lifecycleMu.Unlock()

	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-lifecycleCtx.Done():
		return lifecycleCtx.Err()
	}
}

// stop halts the background poll loop and waits for it to fully exit
// before returning — so a subsequent start() never races a
// still-shutting-down previous goroutine. Safe to call whether or not
// start was ever called, and safe to call more than once.
func (c *configurationClient) stop() {
	c.lifecycleMu.Lock()

	if !c.running {
		c.lifecycleMu.Unlock()

		return
	}

	c.running = false
	cancel := c.lifecycleCancel

	c.lifecycleMu.Unlock()

	cancel()
	c.wg.Wait()
}

// close halts the background poll loop. An alias for stop(): this client
// owns no other resource (no connection pool of its own) that would need
// separate releasing.
func (c *configurationClient) close() {
	c.stop()
}

// getConfig returns the currently cached Configuration, or nil if none
// has ever been successfully fetched.
func (c *configurationClient) getConfig() *Configuration {
	return c.config.Load()
}

// isStale reports whether the cached Configuration should be treated as
// absent: true before any successful fetch, or once maxConfigAge (if set)
// has elapsed since the last successful fetch. Always false once fetched
// when maxConfigAge is unset — a successfully cached Configuration keeps
// serving indefinitely regardless of platform unavailability, per
// feature-evaluation's "Failure Isolation" requirement.
func (c *configurationClient) isStale() bool {
	last := c.lastFetchedAt.Load()
	if last == 0 {
		return true
	}

	if c.maxConfigAge <= 0 {
		return false
	}

	return time.Since(time.Unix(0, last)) > c.maxConfigAge
}

// pollLoop runs strictly within one start()/stop() cycle: ctx is captured
// at the call site (start()) rather than read from a live field, so a
// goroutine from a previous cycle can never observe a later cycle's
// context.
func (c *configurationClient) pollLoop(ctx context.Context) {
	defer c.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		succeeded := c.attemptFetch(ctx)

		delay := c.refreshInterval
		if !succeeded {
			delay = c.nextBackoff()
		} else {
			c.backoff = baseBackoff
		}

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

func (c *configurationClient) nextBackoff() time.Duration {
	delay := c.backoff

	c.backoff *= 2
	if c.backoff > maxBackoff {
		c.backoff = maxBackoff
	}

	return delay
}

func (c *configurationClient) attemptFetch(parentCtx context.Context) bool {
	ctx, cancel := context.WithTimeout(parentCtx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/config", nil)
	if err != nil {
		c.reportError(fmt.Errorf("building GET /v1/config request: %w", err))

		return false
	}

	req.Header.Set("Authorization", "Bearer "+c.credential)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.reportError(fmt.Errorf("GET /v1/config: %w", err))

		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.reportError(fmt.Errorf("GET /v1/config returned status %d", resp.StatusCode))

		return false
	}

	var cfg Configuration
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		c.reportError(fmt.Errorf("decoding GET /v1/config response: %w", err))

		return false
	}

	if cfg.EnvironmentID == "" {
		c.reportError(fmt.Errorf("GET /v1/config response did not match the expected Configuration shape: %w", errEmptyEnvironmentID))

		return false
	}

	c.config.Store(&cfg)
	c.lastFetchedAt.Store(time.Now().UnixNano())
	// Readiness (closing c.ready) happens before the integrator's own
	// callback runs, matching sdk-conformance's "Readiness resolves
	// before callbacks run" scenario.
	c.readyOnce.Do(func() { close(c.ready) })

	if c.onConfigRefreshed != nil {
		safeInvoke(func() { c.onConfigRefreshed(cfg.Version) })
	}

	return true
}

func (c *configurationClient) reportError(err error) {
	if c.onConfigRefreshError != nil {
		safeInvoke(func() { c.onConfigRefreshError(err) })
	}
}
