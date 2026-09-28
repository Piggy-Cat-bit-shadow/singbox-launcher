// Traffic rate: a lightweight up/down speed readout for the menu bar.
//
// This is deliberately NOT the full Traffic Profiler. The profiler joins the
// Clash connection stream with sing-box.log to reconstruct DNS chains,
// per-process attribution and sessions — excellent for a dedicated window,
// but far too much machinery for a menu bar that only needs "how fast, right
// now". Here we poll GET /connections once a second and difference the summed
// byte counters between samples.
//
// Sample, not stream: a 1 Hz timer that stops as soon as the core stops, so a
// closed panel costs nothing.

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	coreservices "singbox-launcher/core/services"
	"strings"
	"sync"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/internal/debuglog"
)

// trafficInterval is how often the totals are sampled. 1 Hz matches the Clash
// API's own polling granularity and is the slowest rate at which a speed
// readout still looks live.
const trafficInterval = time.Second

// trafficTimeout bounds one /connections request so a hung core cannot wedge
// the sampler goroutine forever.
const trafficTimeout = 3 * time.Second

// TrafficSampler polls connection totals and publishes a rate.
type TrafficSampler struct {
	backend *Backend

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	// lastUp/lastDown are the previous sample's cumulative totals; the
	// difference over the elapsed time is the rate.
	lastUp, lastDown int64
	lastAt           time.Time
	haveLast         bool
}

// NewTrafficSampler builds a stopped sampler for a backend.
func NewTrafficSampler(b *Backend) *TrafficSampler {
	return &TrafficSampler{backend: b}
}

// Start begins sampling. Calling Start while already running is a no-op, so
// the frontend can safely ask for it on every connect.
func (t *TrafficSampler) Start() {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.running = true
	// A fresh start must not difference against a stale sample from a
	// previous session, which would report a huge burst.
	t.haveLast = false
	t.mu.Unlock()

	go t.loop(ctx)
}

// Stop halts sampling. Safe to call when not running.
func (t *TrafficSampler) Stop() {
	t.mu.Lock()
	cancel := t.cancel
	t.cancel = nil
	t.running = false
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Running reports whether the sampler is active.
func (t *TrafficSampler) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *TrafficSampler) loop(ctx context.Context) {
	ticker := time.NewTicker(trafficInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sample(ctx)
		}
	}
}

// sample takes one reading and emits a rate event.
//
// Errors are logged at debug level and otherwise ignored: a dropped sample
// means one missing tick, not a user-visible failure, and surfacing an error
// banner every second while the core is starting would be noise.
func (t *TrafficSampler) sample(ctx context.Context) {
	// STOPPED MEANS STOPPED, INCLUDING FOR A SAMPLE ALREADY IN FLIGHT.
	//
	// Stop cancels the loop's context, but a `sample` that had already begun kept running,
	// because nothing re-checked liveness before publishing. It then emitted a traffic rate
	// for a core that had just been stopped — and since the sampler runs exactly while the
	// core is up, the UI reads that as live traffic.
	//
	// Checked on entry so a cancelled run does no work, and again before the emit below,
	// which is the point the event actually escapes.
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	running := t.running
	t.mu.Unlock()
	if !running {
		return
	}

	// The endpoint is resolved the SAME way every other proxy operation resolves it.
	//
	// `clashEndpoint()` reads the configured address out of APIService directly, which is
	// correct for a classic core and WRONG for a daemon: the daemon's Clash API is reached
	// through a VERIFIED transport override, and that verification is the only thing
	// establishing that the endpoint belongs to this daemon rather than to some other
	// Clash-compatible core on the same loopback port. Going straight to the configured
	// address made the sampler the one path that reads from an endpoint the launcher has
	// deliberately not confirmed.
	//
	// `transport()` honours the override, so the sampler now sees exactly what every other
	// proxy operation sees. It returns the same base URL and token the raw request needed,
	// so this is a resolution change rather than a capability loss.
	baseURL, token, ok := t.backend.trafficEndpoint()
	if !ok {
		return
	}

	up, down, err := fetchConnectionTotals(ctx, baseURL, token)
	if err != nil {
		debuglog.DebugLog("traffic sampler: %v", err)
		return
	}

	now := time.Now()

	t.mu.Lock()
	if !t.haveLast {
		// First reading establishes the baseline; a rate needs two points.
		t.lastUp, t.lastDown, t.lastAt, t.haveLast = up, down, now, true
		t.mu.Unlock()
		return
	}
	elapsed := now.Sub(t.lastAt).Seconds()
	prevUp, prevDown := t.lastUp, t.lastDown
	t.lastUp, t.lastDown, t.lastAt = up, down, now
	t.mu.Unlock()

	if elapsed <= 0 {
		return
	}

	// A counter can only go up. A decrease means the core restarted and reset
	// its totals, so report zero for this tick instead of a negative rate.
	upRate := ratePerSecond(up-prevUp, elapsed)
	downRate := ratePerSecond(down-prevDown, elapsed)

	// Re-checked before the emit: the reading above can take up to the request timeout,
	// and the core may have stopped in the meantime.
	t.mu.Lock()
	stillRunning := t.running
	t.mu.Unlock()
	if !stillRunning || ctx.Err() != nil {
		return
	}

	t.backend.emit(protocol.EventTrafficRate, protocol.TrafficRate{
		Up:        upRate,
		Down:      downRate,
		TotalUp:   up,
		TotalDown: down,
		AtUnixMS:  now.UnixMilli(),
	})
}

// ratePerSecond converts a byte delta over an interval into bytes per second.
func ratePerSecond(delta int64, seconds float64) int64 {
	if delta < 0 {
		return 0
	}
	return int64(float64(delta) / seconds)
}

// fetchConnectionTotals sums the byte counters of all live connections.
//
// The totals are cumulative per connection, so summing them and differencing
// successive samples yields the aggregate rate without tracking individual
// connections.
func fetchConnectionTotals(ctx context.Context, baseURL, token string) (up, down int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, trafficTimeout)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/connections"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: trafficTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return 0, 0, fmt.Errorf("connections endpoint returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Only the two counters are decoded: the full connection schema is large
	// and none of it is needed for a rate.
	var payload struct {
		UploadTotal   int64 `json:"uploadTotal"`
		DownloadTotal int64 `json:"downloadTotal"`
		Connections   []struct {
			Upload   int64 `json:"upload"`
			Download int64 `json:"download"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, 0, fmt.Errorf("decode connections: %w", err)
	}

	// The core reports session totals when present; fall back to summing the
	// live connections so the readout still works on cores that omit them.
	if payload.UploadTotal > 0 || payload.DownloadTotal > 0 {
		return payload.UploadTotal, payload.DownloadTotal, nil
	}
	for _, c := range payload.Connections {
		up += c.Upload
		down += c.Download
	}
	return up, down, nil
}

// clashEndpoint resolves the configured Clash API address.
// trafficEndpoint resolves the endpoint the sampler must read from.
//
// It defers to `transport()` rather than to `clashEndpoint()`, because `transport()` is the
// function that knows about the daemon's verified fallback. The resolved transport carries
// the base URL and token, so the sampler reads the same endpoint the rest of the app has
// already agreed to trust — instead of independently deciding, from the configured address
// alone, that whatever answers there is the user's VPN.
func (b *Backend) trafficEndpoint() (baseURL, token string, ok bool) {
	transport, tok := b.transport()
	if !tok {
		return "", "", false
	}
	clash, isClash := transport.(coreservices.ClashTransport)
	if !isClash {
		// A non-Clash transport (a remote pool or chain) has no /connections endpoint to
		// poll. Reporting "nothing to sample" is the honest answer; the previous code
		// would have polled the configured address regardless of which transport the app
		// was actually using.
		return "", "", false
	}
	if clash.BaseURL == "" {
		return "", "", false
	}
	return clash.BaseURL, clash.Token, true
}

// clashEndpoint reports the configured Clash API address, without any verification.
//
// Retained for callers that only need to know whether one is CONFIGURED. Anything that
// talks to the endpoint should resolve it through `transport()` or `trafficEndpoint()`, so
// the daemon's verified fallback is honoured.
func (b *Backend) clashEndpoint() (baseURL, token string, ok bool) {
	if b.ac == nil || b.ac.APIService == nil {
		return "", "", false
	}
	baseURL, token, enabled := b.ac.APIService.GetClashAPIConfig()
	if !enabled || baseURL == "" {
		return "", "", false
	}
	return baseURL, token, true
}
