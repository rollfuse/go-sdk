# github.com/jeanmolossi/growth-ops/packages/sdk-go

The Go SDK for the Growth Operations Platform: evaluates feature flags
locally against cached, versioned configuration (ADR 0004: Evaluate Flags
Locally in SDKs), instead of calling the API synchronously on every
evaluation.

Server-side only — it holds a Service Credential secret, so it targets Go
backends, not untrusted client environments. See
`openspec/specs/sdk-go/spec.md` for its full behavioral contract, and
`packages/sdk-js` for the equivalent Node.js SDK this package mirrors in
idiomatic Go rather than as a direct port.

## Install

```bash
go get github.com/jeanmolossi/growth-ops/packages/sdk-go
```

## Usage

```go
import growthops "github.com/jeanmolossi/growth-ops/packages/sdk-go"

client, err := growthops.NewClient(
    "https://api.growth-ops.example",
    os.Getenv("GROWTH_OPS_CREDENTIAL"), // read wherever you keep secrets; the SDK never reads it itself
)
if err != nil {
    // handle error
}
defer client.Close()

// Optional: block until the first Configuration fetch succeeds before
// serving traffic. Skip this and rely on WithFallback below if you don't
// want to block startup.
if err := client.Start(context.Background()); err != nil {
    // handle error (e.g. ctx cancelled before any fetch succeeded)
}

result, err := client.Evaluate("user_123", "checkout-redesign",
    growthops.WithAttributes(map[string]string{"plan": "enterprise"}),
    growthops.WithFallback(false), // used only if no Configuration is cached yet
)
if err != nil {
    // handle error (ErrConfigNotReady, ErrFlagNotFound)
}

var enabled bool
_ = json.Unmarshal(result.Value, &enabled)
```

`Evaluate`/`EvaluateAll` are synchronous and safe for concurrent use by
multiple goroutines — no network call is made once a Configuration is
cached, per ADR 0004. Exposures produced by a rule-matched evaluation are
reported back to the platform asynchronously, in batches, without adding
latency to the call that produced them.

## Configuration

`NewClient(baseURL, credential string, opts ...Option)` accepts functional
options; all are optional:

| Option | Default | Purpose |
|---|---|---|
| `WithHTTPClient` | `http.DefaultClient` | Override the HTTP client (proxies, mTLS, tracing) |
| `WithRefreshInterval` | `30s` | Interval between successful Configuration refreshes |
| `WithMaxConfigAge` | unset | If set, `Evaluate`/`EvaluateAll` treat the cache as absent once older than this |
| `WithExposureQueueCapacity` | `1000` | Maximum queued-but-unsubmitted exposures |
| `WithExposureBatchSize` | `100` | Queue length that triggers an early submission |
| `WithExposureFlushInterval` | `5s` | Interval between periodic exposure-batch flushes |
| `WithOnConfigRefreshed` | — | Called after each successful refresh |
| `WithOnConfigRefreshError` | — | Called after each failed or invalid refresh |
| `WithOnExposureDropped` | — | Called when exposures are dropped due to a full queue |
| `WithOnExposureSubmitError` | — | Called when a batch of exposures fails to submit |

## Development

```bash
go build ./...
go vet ./...
gofmt -l .
golangci-lint run ./...
go test ./... -race
```

A separate Go module from `apps/api` (its own `go.mod`), so importing it
never pulls in the API's own dependency tree. No dependency beyond the Go
standard library.
