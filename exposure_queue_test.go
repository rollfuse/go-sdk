package rollfuse

import (
	"net/http"
	"net/http/httptest"
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
		capacity: 1,
		onExposureDropped: func(int) {
			callbackRan.Store(true)
			panic("integrator's dropped-exposure callback itself panics")
		},
	})
	defer q.close()

	q.enqueue(sampleQueuedExposure)
	// The queue is now at capacity; this second enqueue triggers the drop
	// path and its panicking callback.
	q.enqueue(sampleQueuedExposure)

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
	q.enqueue(sampleQueuedExposure)
	time.Sleep(30 * time.Millisecond)

	if got := callbackCount.Load(); got < 2 {
		t.Fatalf("expected the panicking submit-error callback to have been invoked more than once, got %d", got)
	}
}
