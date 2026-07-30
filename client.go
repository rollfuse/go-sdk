package growthops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// clientConfig accumulates every Option before Client construction.
type clientConfig struct {
	httpClient            *http.Client
	refreshInterval       time.Duration
	maxConfigAge          time.Duration
	exposureQueueCapacity int
	exposureBatchSize     int
	exposureFlushInterval time.Duration
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
// refreshes (default 30s).
func WithRefreshInterval(d time.Duration) Option {
	return func(cfg *clientConfig) { cfg.refreshInterval = d }
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

	configClient := newConfigurationClient(baseURL, credential, configurationClientOptions{
		refreshInterval:      cfg.refreshInterval,
		maxConfigAge:         cfg.maxConfigAge,
		httpClient:           cfg.httpClient,
		onConfigRefreshed:    cfg.onConfigRefreshed,
		onConfigRefreshError: cfg.onConfigRefreshError,
	})

	queue := newExposureQueue(baseURL, credential, exposureQueueOptions{
		capacity:              cfg.exposureQueueCapacity,
		batchSize:             cfg.exposureBatchSize,
		flushInterval:         cfg.exposureFlushInterval,
		httpClient:            cfg.httpClient,
		onExposureDropped:     cfg.onExposureDropped,
		onExposureSubmitError: cfg.onExposureSubmitError,
	})

	return &Client{configClient: configClient, exposureQueue: queue}, nil
}

// Start begins background Configuration polling and exposure-batch
// flushing. Blocks until the first Configuration fetch succeeds or ctx is
// done; callers that don't want to block startup on it can call
// Start(context.Background()) in a goroutine without waiting on it, and
// rely on Evaluate's WithFallback option until the first fetch lands.
func (c *Client) Start(ctx context.Context) error {
	c.exposureQueue.start()

	return c.configClient.start(ctx)
}

// Close stops background polling and flushing, and submits any remaining
// queued exposures before returning. Always returns nil; the error return
// exists for future-proofing and io.Closer-shaped call sites.
func (c *Client) Close() error {
	c.configClient.close()
	c.exposureQueue.close()

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
