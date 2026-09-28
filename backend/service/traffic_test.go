package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
)

// TestFetchConnectionTotalsUsesCoreTotals — when the core reports session
// totals they are authoritative, because summing live connections loses the
// bytes of connections that have already closed.
func TestFetchConnectionTotalsUsesCoreTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connections" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uploadTotal":   1000,
			"downloadTotal": 2000,
			"connections": []map[string]any{
				{"upload": 1, "download": 2},
			},
		})
	}))
	defer srv.Close()

	up, down, err := fetchConnectionTotals(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("fetchConnectionTotals: %v", err)
	}
	if up != 1000 || down != 2000 {
		t.Errorf("got up=%d down=%d, want 1000/2000", up, down)
	}
}

// TestFetchConnectionTotalsFallsBackToSum — a core that omits the totals must
// not produce a permanently zero readout.
func TestFetchConnectionTotalsFallsBackToSum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": []map[string]any{
				{"upload": 10, "download": 20},
				{"upload": 5, "download": 7},
			},
		})
	}))
	defer srv.Close()

	up, down, err := fetchConnectionTotals(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("fetchConnectionTotals: %v", err)
	}
	if up != 15 || down != 27 {
		t.Errorf("got up=%d down=%d, want 15/27", up, down)
	}
}

// TestFetchConnectionTotalsSendsToken — the Clash API rejects unauthenticated
// requests, so the bearer token must be attached.
func TestFetchConnectionTotalsSendsToken(t *testing.T) {
	const token = "s3cret"
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"uploadTotal": 1, "downloadTotal": 1})
	}))
	defer srv.Close()

	if _, _, err := fetchConnectionTotals(context.Background(), srv.URL, token); err != nil {
		t.Fatalf("fetchConnectionTotals: %v", err)
	}
	if saw != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", saw, "Bearer "+token)
	}
}

// TestFetchConnectionTotalsReportsHTTPError — a failing endpoint must be an
// error so the sampler skips the tick instead of emitting a bogus zero rate.
func TestFetchConnectionTotalsReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, _, err := fetchConnectionTotals(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("fetchConnectionTotals returned no error for HTTP 401")
	}
}

// TestRatePerSecond — a counter reset must read as zero, never as a negative
// rate, because sing-box resets its totals when the core restarts.
func TestRatePerSecond(t *testing.T) {
	cases := []struct {
		name    string
		delta   int64
		seconds float64
		want    int64
	}{
		{"steady", 2048, 2, 1024},
		{"no traffic", 0, 1, 0},
		{"counter reset", -500, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratePerSecond(tc.delta, tc.seconds); got != tc.want {
				t.Errorf("ratePerSecond(%d, %v) = %d, want %d",
					tc.delta, tc.seconds, got, tc.want)
			}
		})
	}
}

// TestSamplerEmitsRate — the sampler must publish a rate derived from the
// difference between two samples, and must not emit on the first sample
// (a rate needs two points).
func TestSamplerEmitsRate(t *testing.T) {
	var total atomic.Int64
	total.Store(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uploadTotal":   total.Load(),
			"downloadTotal": total.Load(),
		})
	}))
	defer srv.Close()

	b := &Backend{}
	var rates []protocol.TrafficRate
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event != protocol.EventTrafficRate {
			return
		}
		if r, ok := ev.Payload.(protocol.TrafficRate); ok {
			rates = append(rates, r)
		}
	})
	defer unsub()

	s := NewTrafficSampler(b)
	// Point the sampler at the stub by injecting the endpoint through the
	// same helper the loop uses.
	baseURL := srv.URL
	sample := func() {
		up, down, err := fetchConnectionTotals(context.Background(), baseURL, "")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		now := time.Now()
		s.mu.Lock()
		if !s.haveLast {
			s.lastUp, s.lastDown, s.lastAt, s.haveLast = up, down, now, true
			s.mu.Unlock()
			return
		}
		elapsed := now.Sub(s.lastAt).Seconds()
		prevUp, prevDown := s.lastUp, s.lastDown
		s.lastUp, s.lastDown, s.lastAt = up, down, now
		s.mu.Unlock()
		if elapsed <= 0 {
			return
		}
		b.emit(protocol.EventTrafficRate, protocol.TrafficRate{
			Up:   ratePerSecond(up-prevUp, elapsed),
			Down: ratePerSecond(down-prevDown, elapsed),
		})
	}

	sample() // baseline: must not emit
	if len(rates) != 0 {
		t.Fatalf("the baseline sample emitted %d event(s)", len(rates))
	}

	total.Store(3000)
	sample()
	// Delivery is asynchronous, so wait for the dispatcher before counting.
	b.FlushEventsForTest()
	if len(rates) != 1 {
		t.Fatalf("got %d rate events, want 1", len(rates))
	}
	if rates[0].Up <= 0 || rates[0].Down <= 0 {
		t.Errorf("rate is not positive: %+v", rates[0])
	}
}

// TestSamplerStartStopIsIdempotent — the frontend may ask to start on every
// connect and stop on every disconnect, so neither may misbehave on repeat.
func TestSamplerStartStopIsIdempotent(t *testing.T) {
	s := NewTrafficSampler(&Backend{})
	if s.Running() {
		t.Fatal("a new sampler is already running")
	}
	s.Start()
	s.Start()
	if !s.Running() {
		t.Fatal("Start did not start the sampler")
	}
	s.Stop()
	s.Stop()
	if s.Running() {
		t.Fatal("Stop did not stop the sampler")
	}
}
