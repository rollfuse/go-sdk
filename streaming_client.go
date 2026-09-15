package rollfuse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// streamBaseBackoff/streamMaxBackoff bound the randomized delay
	// between reconnect attempts (task 5.3), kept separate from the poll
	// loop's own backoff/nextBackoff state so a broken stream never
	// affects polling's own schedule.
	streamBaseBackoff = 1 * time.Second
	streamMaxBackoff  = 30 * time.Second

	// streamDefaultHeartbeatInterval is used only if a hello event is
	// somehow missing or reports a non-positive interval — defensive, the
	// platform always sends a positive one.
	streamDefaultHeartbeatInterval = 30 * time.Second

	// streamMissedHeartbeatMultiplier bounds how long a connection can go
	// without ANY frame (a version event resets the clock exactly like a
	// heartbeat does) before it's treated as silently broken (task 5.3).
	// 3x the disclosed interval tolerates one missed heartbeat's worth of
	// jitter/latency without false-positiving on a connection that's
	// merely slow.
	streamMissedHeartbeatMultiplier = 3

	// streamRequestSetupTimeout bounds only the initial connect + hello
	// handshake, never the long-lived read loop that follows — mirrors
	// requestTimeout's "every network operation carries a deadline"
	// reasoning but can't apply to the read loop itself, which is
	// supposed to block for as long as the connection's own max lifetime.
	streamRequestSetupTimeout = 10 * time.Second
)

// errStreamHandshakeFailed is returned by streamConnectOnce whenever the
// connection could not be established or the hello handshake was invalid,
// covering both explicit refusals (429/503) and any other failure — the
// caller only distinguishes "did we get to a live connection at all",
// never the specific cause, since every case has the identical response:
// fall back to polling and retry later with backoff (task 5.2).
var errStreamHandshakeFailed = errors.New("rollfuse: configuration stream handshake failed")

// streamHelloEvent mirrors the platform's GET /v1/config/stream first
// event, disclosing the heartbeat interval, this connection's own maximum
// lifetime, and the poll interval a connected client should fall back to
// (add-configuration-streaming spec's "The interval is disclosed").
type streamHelloEvent struct {
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
	MaxLifetimeSeconds       int `json:"max_lifetime_seconds"`
	PollIntervalSeconds      int `json:"poll_interval_seconds"`
}

// streamLoop runs for the lifetime of one start()/stop() cycle (ctx is
// captured at the call site exactly like pollLoop's own), repeatedly
// connecting to GET /v1/config/stream and falling back to a randomized
// backoff between attempts. It never touches c.ready: readiness resolves
// entirely from the poll loop's own first successful fetch, per task
// 5.1's "readiness does not depend on the connection succeeding" — this
// loop is purely additive.
func (c *configurationClient) streamLoop(ctx context.Context) {
	defer c.wg.Done()

	backoff := streamBaseBackoff

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.streamConnectOnce(ctx); err != nil {
			c.reportError(fmt.Errorf("configuration stream: %w", err))
		}

		c.streamConnected.Store(false)
		c.streamPollIntervalSeconds.Store(0)

		timer := time.NewTimer(withJitter(backoff))
		backoff *= 2

		if backoff > streamMaxBackoff {
			backoff = streamMaxBackoff
		}

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// streamConnectOnce opens one GET /v1/config/stream connection, reads its
// hello handshake, then blocks reading events until the connection ends
// (cleanly, via ctx, or because it went silently stale — task 5.3) or
// fails outright. Returns a wrapped errStreamHandshakeFailed only when the
// connection never became live (refused, non-2xx, malformed hello);
// returns nil for a connection that connected successfully and later
// ended for any reason, since that's expected, ordinary behavior (the
// server's own max lifetime, a restart, a network blip), not a failure
// worth reporting through onConfigRefreshError on every single
// reconnect.
func (c *configurationClient) streamConnectOnce(parentCtx context.Context) error {
	connectCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	req, err := http.NewRequestWithContext(connectCtx, http.MethodGet, c.baseURL+"/v1/config/stream", nil)
	if err != nil {
		return fmt.Errorf("%w: building request: %v", errStreamHandshakeFailed, err) //nolint:errorlint // wrapping errStreamHandshakeFailed is the sentinel that matters to callers; %v keeps err's text without a second %w
	}

	req.Header.Set("Authorization", "Bearer "+c.credential)
	req.Header.Set("Accept", "text/event-stream")

	// Bounds only the connect-and-handshake phase (task: "every network
	// operation carries a deadline"), never the long-lived read loop that
	// follows: that loop is deliberately unbounded, watched instead by
	// watchForStaleConnection's own heartbeat-driven deadline. Stopped as
	// soon as the hello event is parsed below; if it fires first, it
	// cancels connectCtx exactly like watchForStaleConnection's own
	// cancel does, which unblocks whichever call is currently blocking
	// (Do, or the hello read) with a context-canceled error.
	setupTimer := time.AfterFunc(streamRequestSetupTimeout, cancel)
	defer setupTimer.Stop()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errStreamHandshakeFailed, err) //nolint:errorlint // see above
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Covers both the documented refusals (429 streaming_credential_
		// limit_exceeded, 503 streaming_capacity_exceeded) and any other
		// non-2xx status — every case means the same thing to this
		// client: no stream this time, poll instead (task 5.2).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("%w: status %d", errStreamHandshakeFailed, resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)

	frame, isComment, err := readSSEFrame(reader)
	if err != nil {
		return fmt.Errorf("%w: reading hello: %v", errStreamHandshakeFailed, err) //nolint:errorlint // see above
	}

	if isComment || frame.event != "hello" {
		return fmt.Errorf("%w: first event was not hello", errStreamHandshakeFailed)
	}

	var hello streamHelloEvent
	if err := json.Unmarshal([]byte(frame.data), &hello); err != nil {
		return fmt.Errorf("%w: decoding hello: %v", errStreamHandshakeFailed, err) //nolint:errorlint // see above
	}

	// Hello parsed successfully within the setup deadline — stop the
	// timer now so it can never fire mid-way through the (unbounded)
	// read loop below.
	setupTimer.Stop()

	heartbeatInterval := time.Duration(hello.HeartbeatIntervalSeconds) * time.Second
	if heartbeatInterval <= 0 {
		heartbeatInterval = streamDefaultHeartbeatInterval
	}

	c.streamPollIntervalSeconds.Store(int64(hello.PollIntervalSeconds))
	c.streamConnected.Store(true)

	// pollLoop may already be sleeping out a wait it computed from
	// currentRefreshInterval BEFORE this connection existed (e.g. the
	// platform's own longer advised interval, chosen in the brief window
	// between the client's very first fetch and this connection coming
	// up) — wake it so it recomputes immediately using the now-known
	// reduced interval, rather than sleeping out a stale, longer one
	// (task 5.4). Reuses the same trigger considerVersionNotification
	// uses; pollLoop's resulting fetch is a cheap revalidation (a 304 in
	// the common case) if nothing actually changed, never a correctness
	// issue (mirrors a spurious version notification's own "one wasted
	// fetch" cost).
	select {
	case c.streamFetchTrigger <- struct{}{}:
	default:
	}

	c.watchForStaleConnection(connectCtx, cancel, heartbeatInterval)

	for {
		frame, isComment, err := readSSEFrame(reader)
		if err != nil {
			// Either the connection genuinely broke, or watchForStale
			// Connection canceled connectCtx because no frame arrived
			// within the missed-heartbeat window — both end up here, and
			// both mean the same thing: stop reading, let streamLoop
			// reconnect with backoff.
			return nil
		}

		c.streamLastFrameAt.Store(time.Now().UnixNano())

		if isComment {
			// The heartbeat itself: no payload to act on beyond having
			// already reset streamLastFrameAt above.
			continue
		}

		if frame.event != "version" {
			continue
		}

		version, err := strconv.ParseInt(strings.TrimSpace(frame.data), 10, 64)
		if err != nil {
			continue
		}

		c.considerVersionNotification(version)
	}
}

// watchForStaleConnection starts a goroutine that cancels connectCtx once
// more than streamMissedHeartbeatMultiplier*heartbeatInterval elapses
// since the last frame (heartbeat or version) — the mechanism task 5.3's
// "silently broken connection" detection depends on: canceling the
// request's own context is what unblocks the otherwise-indefinitely-
// blocking bufio.Reader.ReadString call in streamConnectOnce's read loop
// (net/http closes the underlying connection when a request's context is
// canceled, which surfaces as a Read error). The goroutine exits on its
// own once connectCtx ends for any reason (a clean stream end cancels it
// too, via streamConnectOnce's own deferred cancel()).
func (c *configurationClient) watchForStaleConnection(connectCtx context.Context, cancel context.CancelFunc, heartbeatInterval time.Duration) {
	c.streamLastFrameAt.Store(time.Now().UnixNano())

	threshold := heartbeatInterval * streamMissedHeartbeatMultiplier

	c.wg.Add(1)

	go func() {
		defer c.wg.Done()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-connectCtx.Done():
				return
			case <-ticker.C:
				last := time.Unix(0, c.streamLastFrameAt.Load())
				if time.Since(last) > threshold {
					cancel()

					return
				}
			}
		}
	}()
}

// considerVersionNotification records version as the highest one this
// client has ever been notified about, and — only when it is genuinely
// newer than both that record and the Configuration currently cached —
// wakes the poll loop to fetch immediately instead of waiting for its
// next scheduled tick.
//
// The CompareAndSwap loop against streamHighestNotified makes an
// out-of-order or duplicate notification a no-op (task 5.6: a `version:
// 5` arriving after `version: 7` was already acted on never regresses
// anything, since 5 <= 7 fails the check and nothing happens), and the
// separate check against the actually-cached Configuration's own Version
// makes an already-held version a no-op even on a client's very first
// notification (task 5.5).
func (c *configurationClient) considerVersionNotification(version int64) {
	for {
		highest := c.streamHighestNotified.Load()
		if version <= highest {
			return
		}

		if c.streamHighestNotified.CompareAndSwap(highest, version) {
			break
		}
	}

	if cfg := c.config.Load(); cfg != nil && version <= cfg.Version {
		return
	}

	select {
	case c.streamFetchTrigger <- struct{}{}:
	default:
		// A trigger is already pending (pollLoop hasn't consumed it
		// yet); that pending fetch will observe this same-or-newer
		// version once it runs, so a second signal would be redundant.
	}
}

// sseFrame holds one parsed Server-Sent-Events frame's event name (empty
// for the SSE-default "message" type — never actually sent by this
// platform's streaming endpoint, which always names version/hello) and
// data payload (blank-line-joined, matching the SSE spec's multi-"data:"-
// line concatenation rule; this platform never sends more than one data
// line per frame, but joining is still spec-correct).
type sseFrame struct {
	event string
	data  string
}

// readSSEFrame reads one Server-Sent-Events frame (a run of `field: value`
// lines terminated by a blank line) from r. isComment reports whether the
// frame was a bare comment line (`: ...`) — this platform's heartbeat —
// which carries no event/data of its own and is reported separately from
// an ordinary frame so callers don't have to special-case an empty event
// name to distinguish "heartbeat" from "an event with no name". A leading
// run of blank lines before any field line is skipped rather than treated
// as an empty frame, matching real SSE streams which often carry a
// leading blank line as a keep-alive/priming write.
func readSSEFrame(r *bufio.Reader) (frame sseFrame, isComment bool, err error) {
	var event, data strings.Builder

	sawAnyLine := false

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return sseFrame{}, false, err
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if sawAnyLine {
				return sseFrame{event: event.String(), data: data.String()}, false, nil
			}

			continue
		}

		if strings.HasPrefix(line, ":") {
			return sseFrame{}, true, nil
		}

		sawAnyLine = true

		switch {
		case strings.HasPrefix(line, "event:"):
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}

			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}
