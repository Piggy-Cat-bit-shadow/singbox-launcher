// Command jiejiebox-backend is the headless JiejieBox backend.
//
// It speaks the versioned JSON IPC protocol (see backend/protocol) over
// stdin/stdout and is launched by the SwiftUI application as a bundled helper.
// It creates no GUI: stdout carries protocol traffic only, so every log line
// goes to stderr and to the log files under the resolved log directory.
//
// The process is owned by its parent. When stdin closes (the frontend exited
// or crashed) the read loop ends and the backend shuts down, so a frontend
// crash cannot leave an orphaned backend holding the core.
//
// Build with -tags headless (see build/backend_env.sh): that drops the Fyne
// canvas inspector and is what keeps fyne.io out of the dependency graph.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"singbox-launcher/backend/service"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/paths"
)

func main() {
	pathsFlag := flag.Bool("paths", false, "Print resolved data/log paths and exit")
	// -check-config performs the REAL pre-start pipeline without starting
	// anything: build the config from the resolved layout, validate its
	// references, and (when a core is given) run `sing-box check` on it.
	//
	// It exists because the macOS Action built a correct app that could not start
	// a core. Check / Build / Artifact acceptance were all green, and none of them
	// ever produced a configuration, so nothing in CI could notice that the
	// generated config referenced an outbound that did not exist. A build that
	// succeeds is not evidence that a config is usable, and this flag is what lets
	// artifact acceptance say so.
	checkConfigFlag := flag.Bool("check-config", false,
		"Build and validate config.json from the bundled template, then exit (no core started)")
	corePathFlag := flag.String("core", "",
		"Path to a sing-box binary; with -check-config, also run `<core> check -c <config>`")
	flag.Parse()

	exe, err := paths.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jiejiebox-backend: cannot determine executable path: %v\n", err)
		os.Exit(2)
	}

	// Resolve the layout exactly as the GUI build does. The data directory is
	// shared with existing installations, so the frontend switch does not move
	// or migrate user data.
	layout, err := paths.Resolve(exe, os.Getenv, runtime.GOOS, paths.ProbeWritable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jiejiebox-backend: %v\n", err)
		os.Exit(2)
	}

	if *pathsFlag {
		fmt.Printf("Mode: %s\nData: %s\nLogs: %s\n", layout.Mode, layout.Data, layout.Logs)
		return
	}

	if *checkConfigFlag {
		if err := runCheckConfig(layout, *corePathFlag, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "jiejiebox-backend: config pipeline FAILED: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Log files are opened by the controller inside service.New (it owns the
	// FileService), so nothing to initialise here.
	backend, err := service.New(layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jiejiebox-backend: %v\n", err)
		os.Exit(1)
	}

	debuglog.InfoLog("backend: started, pid=%d data=%s", os.Getpid(), layout.Data)

	srv := service.NewServer(backend, os.Stdout)
	srv.Serve(os.Stdin)

	// Serve returns for either reason, and both are legitimate:
	//
	//   1. the frontend asked us to shut down (it also stopped the loop), or
	//   2. stdin reached EOF, the safety net for a frontend that died without
	//      asking — this must not leave an orphan helper owning the core.
	//
	// Shutdown is exactly-once, so reaching it from both paths in one quit is
	// safe and expected. The core's fate follows the existing graceful-exit
	// policy: classic stops it, daemon keeps it unless stop-on-exit is set.
	debuglog.InfoLog("backend: ipc finished, shutting down")
	backend.Shutdown()
}
