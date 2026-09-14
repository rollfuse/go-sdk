package rollfuse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	defaultExposureQueueCapacity = 1000
	defaultExposureBatchSize     = 100
	defaultExposureFlushInterval = 5 * time.Second
	submitTimeout                = 10 * time.Second
	// defaultExposureDedupeWindow is the default width of the window an
	// observation identity (flag, subject, served variation, configuration
	// version) is reported once within — see exposureQueue.dedupeWindow's
	// own doc comment.
	defaultExposureDedupeWindow = 60 * time.Second
)

// exposureEventSubmission is one entry of the POST /v1/exposure-events
// request body, matching apps/api/openapi/openapi.yaml's
// ExposureEventSubmission schema.
type exposureEventSubmission struct {
	FlagKey       string `json:"flag_key"`
	SubjectKey    string `json:"subject_key"`
	VariationKey  string `json:"variation_key"`
	Reason        string `json:"reason"`
	ConfigVersion int64  `json:"config_version"`
	CorrelationID string `json:"correlation_id,omitempty"`
	OccurredAt    string `json:"occurred_at,omitempty"`
}

// queuedExposure is what Client.trackExposure supplies per rule-matched
// evaluation.
type queuedExposure struct {
	FlagKey       string
	SubjectKey    string
	VariationKey  string
	Reason        string
	ConfigVersion int64
}

// exposureIdentity is an observation's dedupe key: flag, subject, served
// variation and configuration version, per design.md's "Deduplication is
// by observation identity" decision. A comparable struct, usable directly
// as a map key.
type exposureIdentity struct {
	FlagKey       string
	SubjectKey    string
	VariationKey  string
	ConfigVersion int64
}

type exposureQueueOptions struct {
	capacity              int
	batchSize             int
	flushInterval         time.Duration
	dedupeWindow          time.Duration
	httpClient            *http.Client
	onExposureDropped     func(count int)
	onExposureSubmitError func(err error)
}

// exposureQueue is a bounded, in-memory, best-effort exposure-submission
// queue, per design.md decision 7 and openspec/specs/sdk-go/spec.md's
// "Asynchronous, Best-Effort Exposure Reporting" requirement. The pending
// buffer is a mutex-protected slice, not a buffered channel: enqueue's
// capacity check and append happen atomically under one lock, so capacity
// bounds the true number of pending events at every instant — a buffered-
// channel design would let a fast-draining background goroutine keep the
// channel under capacity even while events are arriving faster than they
// can be submitted, defeating the bound this queue exists to enforce.
// enqueue never blocks the caller: it never performs I/O and holds the
// lock only for a slice append.
type exposureQueue struct {
	baseURL       string
	credential    string
	capacity      int
	batchSize     int
	flushInterval time.Duration
	// dedupeWindow is the width of the window an observation identity is
	// reported once within (task 7.4, mirroring the Node/browser clients'
	// identical dedupeWindowMs): a repeated evaluation with the same
	// identity inside the window is not re-enqueued; once the window
	// elapses since the identity was last reported, the next matching
	// evaluation is treated as a new observation. A changed variation or
	// configuration version is always a different identity, regardless of
	// timing. This is also the fix for Client enqueuing on every
	// evaluation against a capacity-1000 queue that saturates in
	// milliseconds under real traffic, per proposal.md.
	dedupeWindow          time.Duration
	httpClient            *http.Client
	onExposureDropped     func(count int)
	onExposureSubmitError func(err error)

	mu     sync.Mutex
	buffer []exposureEventSubmission
	// lastReportedAt is when each observation identity was last actually
	// enqueued. Swept on every flush cycle so an identity that stops
	// recurring doesn't pin memory forever — see pruneDedupeWindow.
	lastReportedAt map[exposureIdentity]time.Time
	// droppedSinceLastReport accumulates capacity drops between flush
	// cycles, reported as one aggregated onExposureDropped(count) call per
	// cycle rather than once per dropped event: calling an integrator
	// callback once per event under a sustained drop storm is itself a
	// destabilization risk, the same concern safeInvoke guards against,
	// just at the call-frequency level instead of per-call safety.
	droppedSinceLastReport int

	flushSignal chan struct{}

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	startOnce       sync.Once
	wg              sync.WaitGroup
}

func newExposureQueue(baseURL, credential string, opts exposureQueueOptions) *exposureQueue {
	lifecycleCtx, cancel := context.WithCancel(context.Background())

	capacity := opts.capacity
	if capacity <= 0 {
		capacity = defaultExposureQueueCapacity
	}

	batchSize := opts.batchSize
	if batchSize <= 0 {
		batchSize = defaultExposureBatchSize
	}

	flushInterval := opts.flushInterval
	if flushInterval <= 0 {
		flushInterval = defaultExposureFlushInterval
	}

	dedupeWindow := opts.dedupeWindow
	if dedupeWindow <= 0 {
		dedupeWindow = defaultExposureDedupeWindow
	}

	httpClient := opts.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &exposureQueue{
		baseURL:               baseURL,
		credential:            credential,
		capacity:              capacity,
		batchSize:             batchSize,
		flushInterval:         flushInterval,
		dedupeWindow:          dedupeWindow,
		httpClient:            httpClient,
		onExposureDropped:     opts.onExposureDropped,
		onExposureSubmitError: opts.onExposureSubmitError,
		lastReportedAt:        make(map[exposureIdentity]time.Time),
		flushSignal:           make(chan struct{}, 1),
		lifecycleCtx:          lifecycleCtx,
		lifecycleCancel:       cancel,
	}
}

// start begins the background flush goroutine. Safe to call more than
// once; only the first call starts it.
func (q *exposureQueue) start() {
	q.startOnce.Do(func() {
		q.wg.Add(1)

		go q.run()
	})
}

// close stops the background goroutine, flushing any remaining queued
// events first, and waits for that final flush to complete.
func (q *exposureQueue) close() {
	q.lifecycleCancel()
	q.wg.Wait()
}

// enqueue never blocks and never performs I/O.
//
// Deduplicated by observation identity (flag, subject, served variation,
// configuration version) within dedupeWindow (task 7.4): a repeat within
// the window is silently skipped rather than enqueued again, matching
// sdk-conformance's "The same evaluation repeats" scenario. A changed
// variation or configuration version is always a different identity, so
// it is never skipped regardless of timing.
//
// A full queue drops the new event; drops are accumulated and reported in
// aggregate via onExposureDropped on the next flush cycle (task 7.5)
// rather than growing the queue unbounded or blocking the caller.
func (q *exposureQueue) enqueue(e queuedExposure) {
	identity := exposureIdentity{
		FlagKey:       e.FlagKey,
		SubjectKey:    e.SubjectKey,
		VariationKey:  e.VariationKey,
		ConfigVersion: e.ConfigVersion,
	}
	now := time.Now()

	event := exposureEventSubmission{
		FlagKey:       e.FlagKey,
		SubjectKey:    e.SubjectKey,
		VariationKey:  e.VariationKey,
		Reason:        e.Reason,
		ConfigVersion: e.ConfigVersion,
		CorrelationID: newCorrelationID(),
		OccurredAt:    now.UTC().Format(time.RFC3339),
	}

	q.mu.Lock()

	if lastReportedAt, ok := q.lastReportedAt[identity]; ok && now.Sub(lastReportedAt) < q.dedupeWindow {
		q.mu.Unlock()

		return
	}

	if len(q.buffer) >= q.capacity {
		q.droppedSinceLastReport++

		q.mu.Unlock()

		return
	}

	q.lastReportedAt[identity] = now
	q.buffer = append(q.buffer, event)
	shouldFlush := len(q.buffer) >= q.batchSize

	q.mu.Unlock()

	if shouldFlush {
		q.signalFlush()
	}
}

func (q *exposureQueue) signalFlush() {
	select {
	case q.flushSignal <- struct{}{}:
	default:
	}
}

func (q *exposureQueue) run() {
	defer q.wg.Done()

	ticker := time.NewTicker(q.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			q.flush()
		case <-q.flushSignal:
			q.flush()
		case <-q.lifecycleCtx.Done():
			q.flush()

			return
		}
	}
}

// flush runs once per cycle of run()'s loop (ticker, signalFlush, or the
// final call from close()): sweeps expired dedupe entries, reports any
// capacity drops accumulated since the last cycle, and submits whatever is
// queued, if anything.
func (q *exposureQueue) flush() {
	q.mu.Lock()

	q.pruneDedupeWindowLocked(time.Now())

	dropped := q.droppedSinceLastReport
	q.droppedSinceLastReport = 0

	batch := q.buffer
	q.buffer = nil

	q.mu.Unlock()

	if dropped > 0 && q.onExposureDropped != nil {
		count := dropped

		safeInvoke(func() { q.onExposureDropped(count) })
	}

	if len(batch) == 0 {
		return
	}

	q.submit(batch)
}

// pruneDedupeWindowLocked removes dedupe entries whose window has elapsed,
// bounding memory for identities that stop recurring. Caller must hold mu.
func (q *exposureQueue) pruneDedupeWindowLocked(now time.Time) {
	for identity, reportedAt := range q.lastReportedAt {
		if now.Sub(reportedAt) >= q.dedupeWindow {
			delete(q.lastReportedAt, identity)
		}
	}
}

// submit POSTs batch as a single request. On failure the batch is dropped
// rather than retried or re-queued, per design.md decision 7: retrying
// risks unbounded queue growth under sustained platform unavailability,
// and exposure recording is already best-effort server-side. Uses a fresh
// context (not the queue's own lifecycle context) so the final flush
// triggered by close() can still complete over the network after that
// context is cancelled.
func (q *exposureQueue) submit(batch []exposureEventSubmission) {
	body, err := json.Marshal(struct {
		Events []exposureEventSubmission `json:"events"`
	}{Events: batch})
	if err != nil {
		q.reportSubmitError(fmt.Errorf("marshal exposure batch: %w", err))

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), submitTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.baseURL+"/v1/exposure-events", bytes.NewReader(body))
	if err != nil {
		q.reportSubmitError(fmt.Errorf("building POST /v1/exposure-events request: %w", err))

		return
	}

	req.Header.Set("Authorization", "Bearer "+q.credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.httpClient.Do(req)
	if err != nil {
		q.reportSubmitError(fmt.Errorf("POST /v1/exposure-events: %w", err))

		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		q.reportSubmitError(fmt.Errorf("POST /v1/exposure-events returned status %d", resp.StatusCode))
	}
}

func (q *exposureQueue) reportSubmitError(err error) {
	if q.onExposureSubmitError != nil {
		safeInvoke(func() { q.onExposureSubmitError(err) })
	}
}

// newCorrelationID generates a random UUIDv4-shaped identifier without an
// external dependency, per design.md's "keep the dependency footprint
// small" goal.
func newCorrelationID() string {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
