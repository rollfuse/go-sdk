// Package rollfuse is the Go SDK for rollfuse: it
// evaluates feature flags locally against cached, versioned configuration
// (ADR 0004: Evaluate Flags Locally in SDKs), instead of calling the API
// synchronously on every evaluation.
//
// It targets Go server-side backends: it holds a Service Credential
// secret, so it is not meant for untrusted client environments. See
// openspec/specs/sdk-go/spec.md for its full behavioral contract, and
// packages/sdk-js for the equivalent Node.js SDK this package mirrors in
// idiomatic Go rather than as a direct port.
//
// # Usage
//
//	client, err := rollfuse.NewClient(baseURL, credential)
//	if err != nil {
//		// handle error
//	}
//
//	// Optional: block until the first Configuration fetch succeeds.
//	if err := client.Start(ctx); err != nil {
//		// handle error (e.g. ctx cancelled before any fetch succeeded)
//	}
//	defer client.Close()
//
//	result, err := client.Evaluate(subjectKey, "checkout-redesign",
//		rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}),
//		rollfuse.WithFallback(false),
//	)
//
// Evaluate/EvaluateAll are synchronous and safe for concurrent use by
// multiple goroutines: no network call is made once a Configuration is
// cached. Exposures produced by a rule-matched evaluation are reported
// back to the platform asynchronously, in batches, without adding latency
// to the call that produced them.
//
// Call Close when done to stop background polling/flushing and submit any
// remaining queued exposures; leaving a Client running past its useful
// lifetime leaks its background goroutines.
package rollfuse
