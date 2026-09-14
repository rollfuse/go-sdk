package rollfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// defaultCloseTimeout bounds Close() — see WithCloseTimeout's own doc
// comment.
const defaultCloseTimeout = 5 * time.Second

// clientConfig accumulates every Option before Client construction.
type clientConfig struct {
	httpClient            *http.Client
	refreshInterval       time.Duration
	maxConfigAge          time.Duration
	requestTimeout        time.Duration
	closeTimeout          time.Duration
	exposureQueueCapacity int
	exposureBatchSize     int
	exposureFlushInterval time.Duration
	exposureDedupeWindow  time.Duration
	onConfigRefreshed     func(version int64)
	onConfigRefreshError  func(err error)
	onExposureDropped     func(count int)
	onExposureSubmitError func(err error)
}

// Option configures a Client, passed to NewClient.
type Option func(*clientConfig)

// WithHTTPClient overrides the *http.Client used for both Configuration
// fetches and exposure submission (default http.DefaultClient).
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *clientConfig) { cfg.httpClient = c }
}

// WithRefreshInterval sets the interval between successful Configuration
// refreshes. When not set, the platform's advised
// Configuration.PollIntervalSeconds (from the most recently fetched
// Configuration) is used in preference to this package's own 30s
// default, per sdk-conformance's "The platform advises an interval"
// scenario (task 9.3) — an explicit value here always wins over either.
func WithRefreshInterval(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.refreshInterval = d }
}

// WithRequestTimeout bounds a single GET /v1/config request (default
// 10s). A hung connection is abandoned once it elapses, and the poll loop
// continues on its normal schedule rather than stalling on it
// indefinitely — see configurationClient's own requestTimeout doc
// comment.
func WithRequestTimeout(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.requestTimeout = d }
}

// WithCloseTimeout bounds Close(): it returns once every pending exposure
// has flushed, or once d elapses, whichever comes first, per
// sdk-conformance's "A server process shuts down" scenario. Default 5s.
func WithCloseTimeout(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.closeTimeout = d }
}

// WithMaxConfigAge makes Evaluate/EvaluateAll treat the cached
// Configuration as absent once it is older than d. Off by default: a
// successfully cached Configuration keeps serving indefinitely regardless
// of platform unavailability, per feature-evaluation's "Failure Isolation"
// requirement.
func WithMaxConfigAge(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.maxConfigAge = d }
}

// WithExposureQueueCapacity sets the maximum number of queued-but-
// unsubmitted ExposureEvents (default 1000).
func WithExposureQueueCapacity(n int) Option {
	return func(cfg *clientConfig) { cfg.exposureQueueCapacity = n }
}

// WithExposureBatchSize sets the queue length that triggers an early
// submission batch (default 100).
func WithExposureBatchSize(n int) Option {
	return func(cfg *clientConfig) { cfg.exposureBatchSize = n }
}

// WithExposureFlushInterval sets the interval between periodic exposure-
// batch flushes (default 5s).
func WithExposureFlushInterval(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.exposureFlushInterval = d }
}

// WithExposureDedupeWindow sets the width of the window an observation
// identity (flag, subject, served variation, configuration version) is
// reported once within (default 60s). A repeated evaluation with the same
// identity inside the window is not re-enqueued; once the window elapses
// since the identity was last reported, the next matching evaluation is
// treated as a new observation. A changed variation or configuration
// version is always a different identity, regardless of timing.
func WithExposureDedupeWindow(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.exposureDedupeWindow = d }
}

// WithOnConfigRefreshed registers a callback invoked after each successful
// Configuration refresh, with the new version.
func WithOnConfigRefreshed(fn func(version int64)) Option {
	return func(cfg *clientConfig) { cfg.onConfigRefreshed = fn }
}

// WithOnConfigRefreshError registers a callback invoked after each failed
// or invalid Configuration refresh attempt.
func WithOnConfigRefreshError(fn func(err error)) Option {
	return func(cfg *clientConfig) { cfg.onConfigRefreshError = fn }
}

// WithOnExposureDropped registers a callback invoked when one or more
// ExposureEvents are dropped due to a full queue.
func WithOnExposureDropped(fn func(count int)) Option {
	return func(cfg *clientConfig) { cfg.onExposureDropped = fn }
}

// WithOnExposureSubmitError registers a callback invoked when a batch of
// ExposureEvents fails to submit.
func WithOnExposureSubmitError(fn func(err error)) Option {
	return func(cfg *clientConfig) { cfg.onExposureSubmitError = fn }
}

// evaluateOptions accumulates every EvaluateOption. EvaluateAll ignores
// fallback (no per-flag fallback concept for "evaluate everything").
type evaluateOptions struct {
	attributes  map[string]string
	fallback    any
	hasFallback bool
}

// EvaluateOption configures a single Evaluate/EvaluateAll call.
type EvaluateOption func(*evaluateOptions)

// WithAttributes supplies the subject attributes rule conditions match
// against.
func WithAttributes(attrs map[string]string) EvaluateOption {
	return func(o *evaluateOptions) { o.attributes = attrs }
}

// WithFallback supplies the value Evaluate returns (reason
// "default_fallback") when no Configuration is available yet, per
// "Safe Fallback Behavior". Has no effect on EvaluateAll.
func WithFallback(value any) EvaluateOption {
	return func(o *evaluateOptions) {
		o.fallback = value
		o.hasFallback = true
	}
}

// Client is the platform's Go SDK entry point: fetches and caches a
// Credential-scoped Configuration, evaluates flags against it entirely
// in-process (Evaluate/EvaluateAll, both synchronous and safe for
// concurrent use), and reports rule-matched exposures back to the
// platform asynchronously and best-effort. See
// openspec/specs/sdk-go/spec.md for the full behavioral contract.
type Client struct {
	configClient  *configurationClient
	exposureQueue *exposureQueue
	closeTimeout  time.Duration

	// listenersMu protects configChangeListeners/nextListenerID.
	listenersMu           sync.Mutex
	configChangeListeners map[int]func()
	nextListenerID        int
}

// NewClient constructs a Client for baseURL, authenticated with
// credential. credential MUST be supplied explicitly — it is never read
// from an environment variable or any other ambient source; an empty
// credential returns ErrCredentialRequired.
func NewClient(baseURL, credential string, opts ...Option) (*Client, error) {
	if credential == "" {
		return nil, ErrCredentialRequired
	}

	var cfg clientConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	client := &Client{
		closeTimeout:          cfg.closeTimeout,
		configChangeListeners: make(map[int]func()),
	}
	if client.closeTimeout <= 0 {
		client.closeTimeout = defaultCloseTimeout
	}

	client.configClient = newConfigurationClient(baseURL, credential, configurationClientOptions{
		refreshInterval: cfg.refreshInterval,
		maxConfigAge:    cfg.maxConfigAge,
		requestTimeout:  cfg.requestTimeout,
		httpClient:      cfg.httpClient,
		onConfigRefreshed: func(version int64) {
			if cfg.onConfigRefreshed != nil {
				cfg.onConfigRefreshed(version)
			}

			client.notifyConfigChange()
		},
		onConfigRefreshError: cfg.onConfigRefreshError,
	})

	client.exposureQueue = newExposureQueue(baseURL, credential, exposureQueueOptions{
		capacity:              cfg.exposureQueueCapacity,
		batchSize:             cfg.exposureBatchSize,
		flushInterval:         cfg.exposureFlushInterval,
		dedupeWindow:          cfg.exposureDedupeWindow,
		httpClient:            cfg.httpClient,
		onExposureDropped:     cfg.onExposureDropped,
		onExposureSubmitError: cfg.onExposureSubmitError,
	})

	return client, nil
}

// Subscribe registers listener to be called after each successful
// Configuration refresh that produces a new version (task 10.1: the
// signal an OpenFeature provider wrapping this Client uses to emit its
// own configuration-changed event, without needing to be the one that
// originally constructed the Client with its own WithOnConfigRefreshed).
// Returns an unsubscribe function. Safe for concurrent use.
func (c *Client) Subscribe(listener func()) (unsubscribe func()) {
	c.listenersMu.Lock()
	id := c.nextListenerID
	c.nextListenerID++
	c.configChangeListeners[id] = listener
	c.listenersMu.Unlock()

	return func() {
		c.listenersMu.Lock()
		delete(c.configChangeListeners, id)
		c.listenersMu.Unlock()
	}
}

func (c *Client) notifyConfigChange() {
	c.listenersMu.Lock()

	listeners := make([]func(), 0, len(c.configChangeListeners))
	for _, listener := range c.configChangeListeners {
		listeners = append(listeners, listener)
	}
	c.listenersMu.Unlock()

	for _, listener := range listeners {
		safeInvoke(listener)
	}
}

// Start begins background Configuration polling and exposure-batch
// flushing. Blocks until the first Configuration fetch succeeds or ctx is
// done; callers that don't want to block startup on it can call
// Start(context.Background()) in a goroutine without waiting on it, and
// rely on Evaluate's WithFallback option until the first fetch lands.
// Safe to call again after Stop() (task 8.3): resumes polling and
// reporting rather than leaving the Client permanently inert.
func (c *Client) Start(ctx context.Context) error {
	c.exposureQueue.start()

	return c.configClient.start(ctx)
}

// Stop halts background polling and flushing without submitting queued
// exposures. Safe to call whether or not Start was ever called, and safe
// to call more than once. A later Start resumes both (task 8.3).
func (c *Client) Stop() {
	c.configClient.stop()
	c.exposureQueue.stop()
}

// Close stops background polling and flushing, and submits any remaining
// queued exposures, bounded by WithCloseTimeout (default 5s, task 8.2):
// returns once flushed or once the bound elapses, whichever comes first.
// Always returns nil; the error return exists for future-proofing and
// io.Closer-shaped call sites.
func (c *Client) Close() error {
	done := make(chan struct{})

	go func() {
		c.configClient.close()
		c.exposureQueue.close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(c.closeTimeout):
	}

	return nil
}

// Evaluate evaluates one flag for subjectKey, entirely in-process against
// the cached Configuration. Synchronous and safe for concurrent use by
// multiple goroutines — never performs a network request.
func (c *Client) Evaluate(subjectKey, flagKey string, opts ...EvaluateOption) (EvaluationResult, error) {
	var o evaluateOptions
	for _, opt := range opts {
		opt(&o)
	}

	if c.configClient.isStale() {
		return c.fallbackOrError(flagKey, o, configVersionOf(c.configClient.getConfig()))
	}

	cfg := c.configClient.getConfig()
	if cfg == nil {
		return c.fallbackOrError(flagKey, o, 0)
	}

	flag := findFlag(cfg.Flags, flagKey)
	if flag == nil {
		if o.hasFallback {
			return fallbackResult(flagKey, o.fallback, cfg.Version)
		}

		return EvaluationResult{}, ErrFlagNotFound
	}

	result := EvaluateFlag(*flag, cfg.Version, subjectKey, o.attributes)
	c.trackExposure(subjectKey, result)

	return result, nil
}

// EvaluateAll evaluates every flag in the cached Configuration for
// subjectKey. Synchronous — never performs a network request. Returns
// ErrConfigNotReady if no Configuration is available yet (there is no
// per-flag fallback concept for "evaluate everything").
func (c *Client) EvaluateAll(subjectKey string, opts ...EvaluateOption) ([]EvaluationResult, error) {
	var o evaluateOptions
	for _, opt := range opts {
		opt(&o)
	}

	if c.configClient.isStale() {
		return nil, ErrConfigNotReady
	}

	cfg := c.configClient.getConfig()
	if cfg == nil {
		return nil, ErrConfigNotReady
	}

	results := make([]EvaluationResult, 0, len(cfg.Flags))

	for _, flag := range cfg.Flags {
		result := EvaluateFlag(flag, cfg.Version, subjectKey, o.attributes)
		c.trackExposure(subjectKey, result)
		results = append(results, result)
	}

	return results, nil
}

func (c *Client) fallbackOrError(flagKey string, o evaluateOptions, configVersion int64) (EvaluationResult, error) {
	if o.hasFallback {
		return fallbackResult(flagKey, o.fallback, configVersion)
	}

	return EvaluationResult{}, ErrConfigNotReady
}

func (c *Client) trackExposure(subjectKey string, result EvaluationResult) {
	if !result.TrackExposure {
		return
	}

	c.exposureQueue.enqueue(queuedExposure{
		FlagKey:       result.FlagKey,
		SubjectKey:    subjectKey,
		VariationKey:  result.VariationKey,
		Reason:        string(result.Reason),
		ConfigVersion: result.ConfigVersion,
	})
}

func findFlag(flags []FlagConfig, flagKey string) *FlagConfig {
	for i := range flags {
		if flags[i].FlagKey == flagKey {
			return &flags[i]
		}
	}

	return nil
}

func configVersionOf(cfg *Configuration) int64 {
	if cfg == nil {
		return 0
	}

	return cfg.Version
}

// fallbackResult builds the result for an integrator-supplied fallback:
// never a rule match, and never counted as an exposure. Reuses
// "default_fallback" — the closed EvaluationResult.Reason enum's own
// "could not resolve a real variation" catch-all — rather than inventing
// a value outside the wire contract.
func fallbackResult(flagKey string, value any, configVersion int64) (EvaluationResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return EvaluationResult{}, fmt.Errorf("marshal fallback value: %w", err)
	}

	return EvaluationResult{
		FlagKey:       flagKey,
		VariationKey:  "",
		Value:         raw,
		Reason:        ReasonDefaultFallback,
		ConfigVersion: configVersion,
		TrackExposure: false,
	}, nil
}
