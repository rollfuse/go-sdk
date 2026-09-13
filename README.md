# github.com/rollfuse/go-sdk

> **Extracted from `rollfuse/rollfuse`** (the private platform monorepo) to
> this standalone, public repository. The package identifier stays
> `rollfuse` (`rollfuse.NewClient(...)` etc.) and the client's behavior did
> not change. History prior to the move is preserved (see `git log`).

The Go SDK for rollfuse: evaluates feature flags
locally against cached, versioned configuration (ADR 0004: Evaluate Flags
Locally in SDKs), instead of calling the API synchronously on every
evaluation.

Server-side only — it holds a Service Credential secret, so it targets Go
backends, not untrusted client environments. See
[`rollfuse/js-sdk`](https://github.com/rollfuse/js-sdk)'s `packages/sdk`
for the equivalent Node.js SDK this package mirrors in idiomatic Go rather
than as a direct port.

## Install

```bash
go get github.com/rollfuse/go-sdk
```

## Usage

```go
import rollfuse "github.com/rollfuse/go-sdk"

client, err := rollfuse.NewClient(
    "https://api.rollfuse.com",
    os.Getenv("ROLLFUSE_CREDENTIAL"), // read wherever you keep secrets; the SDK never reads it itself
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
    rollfuse.WithAttributes(map[string]string{"plan": "enterprise"}),
    rollfuse.WithFallback(false), // used only if no Configuration is cached yet
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

A standalone repository and Go module, so importing it never pulls in the
`rollfuse/rollfuse` API's own dependency tree. No dependency beyond the Go
standard library.

`testdata/bucketing-vectors.json` is a checked-in mirror of the
cross-implementation golden-vector fixture whose source of truth is the
private `rollfuse/rollfuse` platform monorepo's
`apps/api/internal/evaluation/domain/testdata/bucketing-vectors.json`
(also mirrored into `rollfuse/js-sdk`'s
`packages/evaluation-core/test/fixtures/bucketing-vectors.json`); see
`bucketing_test.go` for how this copy is kept from silently drifting.
