package services

import (
	"testing"
)

// TestClearMeasurementsClearsErrors is statement 27 (§34 name).
//
// `ClearMeasurements` replaced the measurement map and left `LastPingError` untouched. The
// error map is the OTHER half of the same view: `SetMeasurement` writes both, the UI reads
// both, and `ClearMeasurements` — reached from "clear latency results" — cleared one.
//
// So after clearing, every row shows no delay but the error text from the run the user just
// discarded stays on screen. The data is gone and the complaint about it is not, which reads
// as "these nodes are broken" rather than "these results were cleared".
func TestClearMeasurementsClearsErrors(t *testing.T) {
	// mutableStateLocked creates the scope map lazily, so a zero value is enough.
	svc := &APIService{}

	// Two nodes: one succeeded, one failed with an error.
	svc.SetMeasurement("good", ProxyMeasurementState{
		Delay: 42, Status: MeasurementSuccess,
	})
	svc.SetMeasurement("bad", ProxyMeasurementState{
		Status: MeasurementFailed, Error: "connection refused",
	})

	if got := svc.GetLastPingError("bad"); got == "" {
		t.Fatal("fixture did not record the failure; the test would be vacuous")
	}

	svc.ClearMeasurements()

	if got := svc.GetLastPingError("bad"); got != "" {
		t.Errorf("after clearing the measurements, the failure from the previous run is "+
			"still reported (%q). The row shows no delay but keeps its error, so the user "+
			"sees a complaint about results that no longer exist", got)
	}
	if got := svc.GetLastPingError("good"); got != "" {
		t.Errorf("a stale success-path error survived the clear: %q", got)
	}

	// The measurements themselves must be gone, or the clear did nothing.
	if all := svc.GetMeasurements(); len(all) != 0 {
		t.Errorf("measurements survived the clear: %+v", all)
	}
}
