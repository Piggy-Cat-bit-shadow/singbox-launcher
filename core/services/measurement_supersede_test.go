package services

import "testing"

// TestALateMeasurementCannotOverwriteANewerOne is the assertion that makes
// `ProxyMeasurementState.Generation` load-bearing.
//
// The field existed, was written at two call sites, and was READ BY NOTHING. So the
// supersession it was introduced to provide did not exist, and the race it was meant to close
// was live: clicking a row twice starts two measurements, they can finish in either order,
// and the slower one lands last and overwrites the newer number with a stale one. The user
// then sees a latency they did not just measure, from a request they had already superseded,
// with nothing on screen to indicate it.
//
// A measurement must carry an identity that can be COMPARED. That is the whole point of the
// field, and this test is what holds it to that.
func TestALateMeasurementCannotOverwriteANewerOne(t *testing.T) {
	svc := &APIService{}

	// The newer request is generation 2; the older, slower one is generation 1.
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 20, Status: MeasurementSuccess, Generation: 2,
	}) {
		t.Fatal("the first measurement was rejected; the fixture is wrong")
	}

	if svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 900, Status: MeasurementSuccess, Generation: 1,
	}) {
		t.Fatal("a SUPERSEDED measurement was accepted. Two clicks on the same row race, " +
			"and the slower one lands last: without the generation check the stale number " +
			"overwrites the fresh one and the UI shows a latency the user did not measure")
	}

	got, ok := svc.GetMeasurement("node")
	if !ok {
		t.Fatal("no measurement recorded")
	}
	if got.Delay != 20 {
		t.Fatalf("delay = %d, want the newer 20 — a late reply from an older request "+
			"replaced the current value", got.Delay)
	}

	// A NEWER generation must still be accepted, or the guard would freeze the value.
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 35, Status: MeasurementSuccess, Generation: 3,
	}) {
		t.Fatal("a newer measurement was rejected, so the value could never update")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 35 {
		t.Fatalf("delay = %d, want 35", got.Delay)
	}

	// WITHIN a generation the latest write wins: a retry of the same request must refresh
	// the value rather than being locked out by the first attempt.
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 41, Status: MeasurementSuccess, Generation: 3,
	}) {
		t.Fatal("a same-generation retry was rejected")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 41 {
		t.Fatalf("delay = %d, want 41 from the same-generation retry", got.Delay)
	}
}

// TestSupersededMeasurementDoesNotCorruptTheErrorMap — the discarded write must be
// COMPLETELY discarded.
//
// `SetMeasurement` writes two maps: the value and the error text. A guard that rejected the
// value but still applied the error would leave a row showing a fresh delay alongside a
// failure message from the request that lost — a combination that never happened.
func TestSupersededMeasurementDoesNotCorruptTheErrorMap(t *testing.T) {
	svc := &APIService{}

	svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 20, Status: MeasurementSuccess, Generation: 5,
	})
	if err := svc.GetLastPingError("node"); err != "" {
		t.Fatalf("fixture recorded an error: %q", err)
	}

	// The losing request failed. Its error must not appear.
	svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Status: MeasurementFailed, Error: "connection refused", Generation: 4,
	})

	if err := svc.GetLastPingError("node"); err != "" {
		t.Errorf("a SUPERSEDED measurement still wrote its error (%q). The value was "+
			"correctly rejected and the error was not, so the row shows a fresh delay "+
			"beside a failure that did not produce it", err)
	}
}

// TestUnnumberedMeasurementsDoNotSupersedeEachOther — generation 0 means "no identity".
//
// Group tests and other writers do not always have a single-test generation, and a zero must
// not be treated as "older than everything", or a group run's results would be silently
// discarded whenever a hand test had ever run.
func TestUnnumberedMeasurementsDoNotSupersedeEachOther(t *testing.T) {
	svc := &APIService{}

	svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 10, Status: MeasurementSuccess, Generation: 0,
	})
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 50, Status: MeasurementSuccess, Generation: 0,
	}) {
		t.Fatal("a second unnumbered measurement was rejected, so a group run could never " +
			"update a node a hand test had touched")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 50 {
		t.Fatalf("delay = %d, want 50", got.Delay)
	}
}
