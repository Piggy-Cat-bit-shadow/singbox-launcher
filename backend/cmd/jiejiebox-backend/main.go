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

	// stdin closed: the frontend is gone. Stop the core according to the
	// existing graceful-exit policy rather than leaving it running unowned.
	debuglog.InfoLog("backend: stdin closed, shutting down")
	backend.Shutdown()
}
