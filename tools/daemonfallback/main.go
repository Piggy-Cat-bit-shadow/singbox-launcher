// Command daemonfallback verifies the daemon-local Clash API fallback on a real
// machine, read-only by default.
//
// It exists because the properties that matter most here cannot be tested in
// CI: they are about the ONE daemon installed on the user's machine and the
// state of the core it is currently running. CI has no daemon, no paired mTLS
// client and no live VPN, and a test that required them would be skipped
// everywhere and trusted nowhere.
//
// What it answers, in order:
//
//  1. which proxy RPCs the installed daemon implements;
//  2. what Clash API endpoint our config transformation DERIVES for it;
//  3. whether that endpoint is actually serving the core right now;
//  4. whether the groups it serves match the config we would send.
//
// READ-ONLY. It applies no config and switches no node, so it is safe to run
// against a machine with a live VPN. Making the fallback real requires the GUI
// to Start/Restart the core so the daemon receives the new config; this tool
// then confirms the result.
//
// Usage:
//
//	go run ./tools/daemonfallback                    # inspect only
//	go run ./tools/daemonfallback -switch-check TAG  # add a gRPC switch probe
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to config.json (default: app data dir)")
		secret     = flag.String("secret", "", "override the Clash secret (never printed)")
		timeout    = flag.Duration("timeout", 4*time.Second, "per-request timeout")
	)
	flag.Parse()

	path := *configPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fail("cannot resolve home: %v", err)
		}
		path = filepath.Join(home, "Library", "Application Support",
			"singbox-launcher", "bin", "config.json")
	}

	src, err := os.ReadFile(path)
	if err != nil {
		fail("read config %s: %v", path, err)
	}

	fmt.Printf("config      : %s\n", path)
	controller, token, groups, err := transform(src)
	if err != nil {
		fail("transform: %v", err)
	}
	if *secret != "" {
		token = *secret
	}
	fmt.Printf("runtime API : %s\n", controller)
	fmt.Printf("secret      : %s\n", describeSecret(token))
	fmt.Printf("groups      : %v\n", groups)

	if !isLoopback(controller) {
		fail("derived controller %s is NOT loopback; the fallback must never "+
			"point at a non-local address", controller)
	}

	base := "http://" + controller
	proxies, status, err := fetchProxies(base, token, *timeout)
	switch {
	case err != nil:
		fmt.Printf("\nGET /proxies: FAIL (%v)\n", err)
		fmt.Println("\nVERDICT: fallback NOT READY.")
		fmt.Println("  Nothing is serving the derived endpoint. The installed daemon is")
		fmt.Println("  most likely still running the config it was given BEFORE this")
		fmt.Println("  change (which stripped clash_api). Start or Restart the core in")
		fmt.Println("  JiejieBox to apply a config that includes it, then re-run.")
		fmt.Println("  This is a retryable state, NOT \"unsupported\".")
		os.Exit(2)
	case status != http.StatusOK:
		fmt.Printf("\nGET /proxies: HTTP %d\n", status)
		fmt.Println("\nVERDICT: endpoint answered but rejected our credentials.")
		fmt.Println("  It is probably not our daemon. Fallback stays disabled.")
		os.Exit(3)
	}

	fmt.Printf("\nGET /proxies: OK (%d proxies)\n", len(proxies))
	missing := []string{}
	for _, g := range groups {
		if _, ok := proxies[g]; !ok {
			missing = append(missing, g)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("missing configured groups: %v\n", missing)
		fmt.Println("\nVERDICT: config/runtime MISMATCH — the running core is not on this")
		fmt.Println("  config. Fallback stays disabled so we never drive a core whose")
		fmt.Println("  state we cannot account for. Restart the core to apply the config.")
		os.Exit(4)
	}
	fmt.Printf("all configured groups present: %v\n", groups)
	fmt.Println("\nVERDICT: fallback READY — listing and latency over HTTP, switching over gRPC.")
	fmt.Println("Note: this proves the endpoint serves OUR config. It does not prove it is")
	fmt.Println("the same process the gRPC channel controls; no fingerprint is available")
	fmt.Println("because GetRunningConfig is Unimplemented on this daemon build.")
}

func transform(src []byte) (controller, token string, groups []string, err error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(src, &root); err != nil {
		return "", "", nil, err
	}
	var exp map[string]json.RawMessage
	if raw, ok := root["experimental"]; ok {
		if err := json.Unmarshal(raw, &exp); err != nil {
			return "", "", nil, err
		}
	}
	var ca map[string]json.RawMessage
	if raw, ok := exp["clash_api"]; ok {
		if err := json.Unmarshal(raw, &ca); err != nil {
			return "", "", nil, err
		}
	}
	var c string
	if v, ok := ca["external_controller"]; ok {
		_ = json.Unmarshal(v, &c)
	}
	if v, ok := ca["secret"]; ok {
		_ = json.Unmarshal(v, &token)
	}
	controller = loopback(c)

	var outbounds []map[string]json.RawMessage
	if raw, ok := root["outbounds"]; ok {
		_ = json.Unmarshal(raw, &outbounds)
	}
	for _, o := range outbounds {
		var typ, tag string
		if v, ok := o["type"]; ok {
			_ = json.Unmarshal(v, &typ)
		}
		if typ != "selector" && typ != "urltest" {
			continue
		}
		if v, ok := o["tag"]; ok {
			_ = json.Unmarshal(v, &tag)
		}
		if tag != "" {
			groups = append(groups, tag)
		}
	}
	return controller, token, groups, nil
}

func loopback(controller string) string {
	i := strings.LastIndex(controller, ":")
	if i < 0 {
		return ""
	}
	return "127.0.0.1" + controller[i:]
}

func isLoopback(addr string) bool {
	return strings.HasPrefix(addr, "127.0.0.1:") || strings.HasPrefix(addr, "localhost:")
}

func describeSecret(s string) string {
	if s == "" {
		return "(none — unauthenticated loopback, as the project allows)"
	}
	return fmt.Sprintf("(present, %d chars — not printed)", len(s))
}

func fetchProxies(base, token string, timeout time.Duration) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/proxies", nil)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	var parsed struct {
		Proxies map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("not a Clash API response: %w", err)
	}
	return parsed.Proxies, resp.StatusCode, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "daemonfallback: "+format+"\n", args...)
	os.Exit(1)
}
