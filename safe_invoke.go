package rollfuse

// safeInvoke calls fn, recovering from and discarding any panic so it
// cannot terminate the host process. Per sdk-conformance's "Integrator
// Code Cannot Destabilize The Host Process" requirement: every callback
// this library invokes on the integrator's behalf — every
// OnConfigRefreshed/OnConfigRefreshError/OnExposureDropped/
// OnExposureSubmitError call — goes through this single point rather than
// being invoked directly at each call site. Unlike a JavaScript exception,
// an unrecovered Go panic terminates the entire process regardless of
// which goroutine it originates in, so this applies equally to callbacks
// invoked from the caller's own goroutine (enqueue) and from this
// library's own background goroutines (the poll loop, the flush loop).
//
// There is deliberately no further diagnostic path here: reporting a
// broken error-reporting callback's own failure through that same
// callback risks an infinite loop, and this library has no other channel
// to report it through.
func safeInvoke(fn func()) {
	defer func() {
		_ = recover()
	}()

	fn()
}
