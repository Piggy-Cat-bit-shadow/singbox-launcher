//go:build headless

// Package backend is the headless JiejieBox backend.
//
// The headless build tag is required: it selects the stub for the Fyne canvas
// inspector, which is the last thing tying the backend to fyne.io. Without the
// tag the backend still compiles but pulls the whole GUI toolkit into its
// dependency graph — see docs/BACKEND_PROTOCOL.md.
package backend
