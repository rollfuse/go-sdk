package rollfuse

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var sampleQueuedExposure = queuedExposure{
	FlagKey:       "checkout-redesign",
	SubjectKey:    "user_1",
	VariationKey:  "on",
	Reason:        "rule_match",
	ConfigVersion: 3,
}

// TestExposureQueue_PanickingOnExposureDroppedDoesNotCrash exercises
// harden-sdk-runtime task 3.4: a panicking OnExposureDropped must not
// terminate the process. enqueue runs synchronously in the caller's own
// goroutine (never a background one), so this also proves the isolation
// holds there, not only for the background poll/flush loops.
func TestExposureQueue_PanickingOnExposureDroppedDoesNotCrash(t *testing.T) {
	var callbackRan atomic.Bool

	q := newExposureQueue("http://unused.test", "cred", exposureQueueOptions{
		capacity:      1,
		flushInterval: 10 * time.Millisecond,
		onExposureDropped: func(int) {
			callbackRan.Store(true)
			panic("integrator's dropped-exposure callback itself panics")
		},
	})

	q.start()
	defer q.close()

	q.enqueue(sampleQueuedExposure)
	// A distinct identity: the same one would now be deduplicated (task
	// 7.4) rather than reach the capacity check at all. This one still
	// finds the queue at capacity and triggers the drop path.
	distinctExposure := sampleQueuedExposure
	distinctExposure.SubjectKey = "user_2"
	q.enqueue(distinctExposure)

	// Drops are now aggregated and reported once per flush cycle (task
	// 7.5), not synchronously inside enqueue().
	time.Sleep(30 * time.Millisecond)

	if !callbackRan.Load() {
		t.Fatal("expected the panicking callback to have been invoked")
	}
}

// TestExposureQueue_PanickingOnExposureSubmitErrorDoesNotCrash is the same
// proof for the submit-error callback (task 3.4), invoked from the
// background flush loop, across more than one flush.
func TestExposureQueue_PanickingOnExposureSubmitErrorDoesNotCrash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var callbackCount atomic.Int64

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		flushInterval: 20 * time.Millisecond,
		onExposureSubmitError: func(error) {
			callbackCount.Add(1)
			panic("integrator's submit-error callback itself panics")
		},
	})
	defer q.close()

	q.start()
	q.enqueue(sampleQueuedExposure)

	time.Sleep(30 * time.Millisecond)
	// A distinct subject: the same identity as the first enqueue would now
	// be deduplicated (task 7.4) within the default window, never
	// reaching a second submit attempt.
	distinctExposure := sampleQueuedExposure
	distinctExposure.SubjectKey = "user_2"
	q.enqueue(distinctExposure)
	time.Sleep(30 * time.Millisecond)

	if got := callbackCount.Load(); got < 2 {
		t.Fatalf("expected the panicking submit-error callback to have been invoked more than once, got %d", got)
	}
}

// recordingExposureServer accepts POST /v1/exposure-events and records
// every submitted batch, safe for concurrent access from the queue's
// background flush goroutine and the test's own assertions.
type recordingExposureServer struct {
	mu      sync.Mutex
	batches [][]exposureEventSubmission
}

func (s *recordingExposureServer) handler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []exposureEventSubmission `json:"events"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	s.mu.Lock()
	s.batches = append(s.batches, body.Events)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *recordingExposureServer) totalEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for _, batch := range s.batches {
		total += len(batch)
	}

	return total
}

// TestExposureQueue_RepeatedIdenticalEvaluationsProduceOneExposure
// exercises harden-sdk-runtime task 7.4/7.1 for the Go client: enqueuing
// the same observation identity repeatedly within the dedupe window
// submits it once. Manually verified: reverting enqueue to skip the
// dedupe check (always append) made this test submit 3 events instead of
// 1; restored before committing.
func TestExposureQueue_RepeatedIdenticalEvaluationsProduceOneExposure(t *testing.T) {
	recorder := &recordingExposureServer{}

	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		flushInterval: 20 * time.Millisecond,
		dedupeWindow:  time.Minute,
	})
	q.start()

	q.enqueue(sampleQueuedExposure)
	q.enqueue(sampleQueuedExposure)
	q.enqueue(sampleQueuedExposure)

	q.close()

	if got := recorder.totalEvents(); got != 1 {
		t.Fatalf("expected 1 submitted event, got %d", got)
	}
}

// TestExposureQueue_ChangedVariationOrConfigVersionIsANewIdentity
// exercises task 7.4/7.2: a changed served variation, and separately a
// changed configuration version, each produce a distinct exposure even
// within the dedupe window.
func TestExposureQueue_ChangedVariationOrConfigVersionIsANewIdentity(t *testing.T) {
	recorder := &recordingExposureServer{}

	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		flushInterval: 20 * time.Millisecond,
		dedupeWindow:  time.Minute,
	})
	q.start()

	changedVariation := sampleQueuedExposure
	changedVariation.VariationKey = "off"

	changedVersion := sampleQueuedExposure
	changedVersion.ConfigVersion = sampleQueuedExposure.ConfigVersion + 1

	q.enqueue(sampleQueuedExposure)
	q.enqueue(changedVariation)
	q.enqueue(changedVersion)

	q.close()

	if got := recorder.totalEvents(); got != 3 {
		t.Fatalf("expected 3 distinct submitted events, got %d", got)
	}
}

// TestExposureQueue_DedupeWindowElapsedReportsAgain exercises task 7.4/7.1:
// once dedupeWindow has elapsed since an identity was last reported, the
// same identity is treated as a new observation.
func TestExposureQueue_DedupeWindowElapsedReportsAgain(t *testing.T) {
	recorder := &recordingExposureServer{}

	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		flushInterval: 10 * time.Millisecond,
		dedupeWindow:  20 * time.Millisecond,
	})
	q.start()

	q.enqueue(sampleQueuedExposure)
	time.Sleep(50 * time.Millisecond) // outlasts both the dedupe window and a flush cycle
	q.enqueue(sampleQueuedExposure)

	q.close()

	if got := recorder.totalEvents(); got != 2 {
		t.Fatalf("expected 2 submitted events once the dedupe window elapsed between them, got %d", got)
	}
}

// TestExposureQueue_CapacityDropsAreAggregatedPerFlushCycle exercises task
// 7.5: capacity drops accumulate locally and are reported as one
// aggregated onExposureDropped(count) call per flush cycle, rather than
// once per dropped event. Manually verified: reverting the accumulator to
// a direct safeInvoke(onExposureDropped, 1) per drop made this test
// observe 5 separate calls instead of one call with 5; restored before
// committing.
func TestExposureQueue_CapacityDropsAreAggregatedPerFlushCycle(t *testing.T) {
	recorder := &recordingExposureServer{}

	server := httptest.NewServer(http.HandlerFunc(recorder.handler))
	defer server.Close()

	var (
		dropCalls atomic.Int64
		lastCount atomic.Int64
	)

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		capacity:      1,
		flushInterval: 2 * time.Second, // long enough that all drops land in one cycle
		onExposureDropped: func(n int) {
			dropCalls.Add(1)
			lastCount.Store(int64(n))
		},
	})
	q.start()

	q.enqueue(sampleQueuedExposure) // occupies the one slot

	for i := 0; i < 5; i++ {
		overflow := sampleQueuedExposure
		overflow.SubjectKey = "overflow_" + string(rune('a'+i))
		q.enqueue(overflow)
	}

	if dropCalls.Load() != 0 {
		t.Fatalf("expected no drop callback yet (aggregation happens on the next flush cycle), got %d calls", dropCalls.Load())
	}

	q.close()

	if got := dropCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 aggregated drop callback, got %d", got)
	}

	if got := lastCount.Load(); got != 5 {
		t.Fatalf("expected the aggregated callback to report 5 drops, got %d", got)
	}
}

// TestExposureQueue_ConcurrentFlushesAreSerialized exercises task 7.6 for
// the Go client: the single run() goroutine processes ticker, signalFlush
// and close sequentially by construction, so flush() (and therefore
// submit()) can never execute concurrently with itself. Proven here under
// concurrent enqueue() calls from many goroutines while flushes are
// firing on a fast timer: the recording server must never observe two
// requests in flight at once.
func TestExposureQueue_ConcurrentFlushesAreSerialized(t *testing.T) {
	var (
		inFlight               atomic.Int32
		maxObservedConcurrency atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)

		for {
			max := maxObservedConcurrency.Load()
			if n <= max || maxObservedConcurrency.CompareAndSwap(max, n) {
				break
			}
		}

		time.Sleep(5 * time.Millisecond) // wide enough to overlap with a concurrent flush, if one were possible
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	q := newExposureQueue(server.URL, "cred", exposureQueueOptions{
		batchSize:     5,
		flushInterval: 2 * time.Millisecond,
	})
	q.start()

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)

		go func(g int) {
			defer wg.Done()

			for i := 0; i < 20; i++ {
				e := sampleQueuedExposure
				e.SubjectKey = "user_" + string(rune('a'+g)) + string(rune('0'+i%10))
				q.enqueue(e)
			}
		}(g)
	}

	wg.Wait()

	q.close()

	if got := maxObservedConcurrency.Load(); got > 1 {
		t.Fatalf("expected at most 1 concurrent submit request, observed %d", got)
	}
}
