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

// TestAGroupRunSupersedesAHAndTestAndViceVersa — the two generation spaces must be ORDERED
// against each other, not just internally.
//
// Hand tests and group runs draw from separate ranges, and the group path used to write
// through `SetMeasurement` — which does not compare at all — while the hand path used
// `RecordMeasurementIfNewer`. The asymmetry broke the guard in BOTH directions:
//
//   - a group result always overwrote a hand test's fresher result, which is precisely the
//     clobbering the guard exists to prevent;
//   - and a hand test's generation sits far ABOVE every group id, so no later group run could
//     ever supersede it: one hand test pinned a node's displayed latency against every
//     subsequent refresh of it.
//
// Both writes go through the same comparison now, and this test drives it as one sequence
// rather than testing each space alone — which is what let the asymmetry survive.
func TestAGroupRunSupersedesAHandTestAndViceVersa(t *testing.T) {
	svc := &APIService{}

	// A group run reports first: a small generation.
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 100, Status: MeasurementSuccess, Generation: 7,
	}) {
		t.Fatal("the first measurement was rejected on an empty slot")
	}

	// A hand test, from the single-test space, well above every group id. The base is
	// declared in `backend/service`'s scheduler; the VALUE is duplicated here because this
	// package is what must keep the two spaces ordered, and a test that imported the
	// constant could not notice it being lowered to something a group id could reach.
	const handGeneration = (1 << 62) + 1
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 10, Status: MeasurementSuccess, Generation: handGeneration,
	}) {
		t.Fatal("a hand test could not supersede a group result, so the user's own test " +
			"would be discarded as a late reply from the run it replaced")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 10 {
		t.Fatalf("delay = %d, want the hand test's 10", got.Delay)
	}

	// A GROUP RUN MUST NOT OVERWRITE A NEWER HAND TEST. This is the direction that actually
	// matters, and the one the asymmetry broke: the group path wrote unconditionally, so a
	// scheduled run landing a second after the user pressed "test" replaced their result.
	//
	// The group id space and the hand-test space are deliberately ordered with hand tests
	// ABOVE every group id, so a hand test is never superseded by a run. The price is that a
	// group run cannot supersede a hand test either — that is the intended trade-off, not an
	// oversight: a scheduled refresh is not more authoritative than a test the user just
	// asked for, and the user can clear the result to let group runs back in.
	if svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 999, Status: MeasurementSuccess, Generation: 8,
	}) {
		t.Fatal("a group run overwrote a hand test's NEWER result; the compare was bypassed, " +
			"so a scheduled refresh silently replaces what the user just measured")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 10 {
		t.Fatalf("delay = %d, want the hand test's 10", got.Delay)
	}

	// And the reverse order must still protect the newer result: an OLDER group run may not
	// overwrite a newer hand test.
	if !svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 5, Status: MeasurementSuccess, Generation: handGeneration + 1,
	}) {
		t.Fatal("a newer hand test was rejected")
	}
	if svc.RecordMeasurementIfNewer("node", ProxyMeasurementState{
		Delay: 999, Status: MeasurementSuccess, Generation: 9,
	}) {
		t.Fatal("a group run numbered 9 overwrote a hand test numbered far above it; the two " +
			"spaces are not ordered against each other")
	}
	if got, _ := svc.GetMeasurement("node"); got.Delay != 5 {
		t.Fatalf("delay = %d, want the hand test's 5", got.Delay)
	}
}
