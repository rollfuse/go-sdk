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

type exposureQueueOptions struct {
	capacity              int
	batchSize             int
	flushInterval         time.Duration
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
	baseURL               string
	credential            string
	capacity              int
	batchSize             int
	flushInterval         time.Duration
	httpClient            *http.Client
	onExposureDropped     func(count int)
	onExposureSubmitError func(err error)

	mu     sync.Mutex
	buffer []exposureEventSubmission

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
		httpClient:            httpClient,
		onExposureDropped:     opts.onExposureDropped,
		onExposureSubmitError: opts.onExposureSubmitError,
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

// enqueue never blocks and never performs I/O: a full queue drops the
// event and reports it via onExposureDropped rather than growing unbounded
// or blocking the caller.
func (q *exposureQueue) enqueue(e queuedExposure) {
	event := exposureEventSubmission{
		FlagKey:       e.FlagKey,
		SubjectKey:    e.SubjectKey,
		VariationKey:  e.VariationKey,
		Reason:        e.Reason,
		ConfigVersion: e.ConfigVersion,
		CorrelationID: newCorrelationID(),
		OccurredAt:    time.Now().UTC().Format(time.RFC3339),
	}

	q.mu.Lock()

	if len(q.buffer) >= q.capacity {
		q.mu.Unlock()

		if q.onExposureDropped != nil {
			q.onExposureDropped(1)
		}

		return
	}

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

func (q *exposureQueue) flush() {
	q.mu.Lock()

	if len(q.buffer) == 0 {
		q.mu.Unlock()

		return
	}

	batch := q.buffer
	q.buffer = nil

	q.mu.Unlock()

	q.submit(batch)
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
		q.onExposureSubmitError(err)
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
