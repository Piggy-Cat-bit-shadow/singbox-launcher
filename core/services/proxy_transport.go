package services

import (
	"context"
	"errors"
	"fmt"

	"singbox-launcher/api"
)

// ErrProxyListUnsupported reports that the ACTIVE ENGINE cannot list proxies at
// all — not that a particular read failed.
//
// The distinction matters because the two need opposite UI treatment. A failed
// read is an error the user can retry; an unsupported capability is a fact about
// the engine that retrying will never change, so showing it as a red error
// banner blames the user for something they cannot act on and hides the step
// that would actually help.
//
// The concrete case: a daemon built without the group RPC answers
// codes.Unimplemented. The launcher's proto declares GetGroups, so the call
// compiles and the transport looks healthy right up to the moment the daemon
// says it has never heard of the method. Detecting it here — at the only layer
// that talks to the daemon — is what lets every caller above treat it as a
// capability rather than a failure.
var ErrProxyListUnsupported = errors.New("the active engine does not support listing proxies")

// ErrProxySwitchUnsupported reports that the active engine cannot SELECT a proxy,
// while it may still be able to list them.
//
// Kept separate from the list sentinel because an engine can genuinely have one
// without the other, and collapsing them disables working features. A daemon
// that serves the group read but not SelectOutbound can still show its nodes;
// the user simply cannot switch from here. Reporting that as "listing
// unsupported" would hide a list that works.
var ErrProxySwitchUnsupported = errors.New("the active engine does not support switching proxies")

// ErrProxyLatencyUnsupported reports that the active engine cannot MEASURE
// latency, while listing and switching may both work.
//
// This is the case the coarse single-sentinel model got wrong most often: the
// whole proxy page was treated as broken because one action was missing. An
// engine without the URL-test RPC still lists and still switches; only the test
// column is unavailable, and only that should be disabled.
var ErrProxyLatencyUnsupported = errors.New("the active engine does not support measuring proxy latency")

// ProxyCapability identifies which proxy action a capability error refers to.
//
// Callers switch on this rather than on error text or on the backend mode, so
// the UI never has to guess what an engine can do from a string or from knowing
// whether it is talking to a daemon.
type ProxyCapability string

const (
	// CapabilityList — reading the proxies of a group.
	CapabilityList ProxyCapability = "list"
	// CapabilitySwitch — selecting a proxy inside a selector group.
	CapabilitySwitch ProxyCapability = "switch"
	// CapabilityTest — measuring proxy latency (single node or whole group).
	CapabilityTest ProxyCapability = "test"
)

// ProxyCapabilityError is a capability report carrying the action it concerns.
//
// It wraps one of the sentinels above, so both work: errors.Is(err,
// ErrProxyLatencyUnsupported) and a structured switch on Capability. The
// sentinels keep existing callers working; the struct answers "which action?"
// without string matching.
type ProxyCapabilityError struct {
	Capability ProxyCapability
	// Err is one of ErrProxyListUnsupported / ErrProxySwitchUnsupported /
	// ErrProxyLatencyUnsupported.
	Err error
}

func (e *ProxyCapabilityError) Error() string {
	if e == nil || e.Err == nil {
		return "proxy action unsupported"
	}
	return e.Err.Error()
}

func (e *ProxyCapabilityError) Unwrap() error { return e.Err }

// NewProxyCapabilityError builds a capability report for an action.
func NewProxyCapabilityError(c ProxyCapability) error {
	switch c {
	case CapabilityList:
		return &ProxyCapabilityError{Capability: c, Err: ErrProxyListUnsupported}
	case CapabilitySwitch:
		return &ProxyCapabilityError{Capability: c, Err: ErrProxySwitchUnsupported}
	case CapabilityTest:
		return &ProxyCapabilityError{Capability: c, Err: ErrProxyLatencyUnsupported}
	default:
		return &ProxyCapabilityError{Capability: c, Err: ErrProxyListUnsupported}
	}
}

// IsProxyCapabilityError reports whether err is any proxy capability report.
func IsProxyCapabilityError(err error) bool {
	return errors.Is(err, ErrProxyListUnsupported) ||
		errors.Is(err, ErrProxySwitchUnsupported) ||
		errors.Is(err, ErrProxyLatencyUnsupported)
}

// ProxyTransport abstracts the wire used for proxy-group operations. The
// classic engine talks to the Clash HTTP API embedded in the running core;
// the daemon engine talks gRPC to the lxd daemon. APIService и Servers-tab
// ходят только через этот интерфейс, поэтому смена движка не трогает UI.
type ProxyTransport interface {
	// GroupProxies returns the proxies of a selector group plus the tag the
	// group has currently selected ("now").
	GroupProxies(group string) ([]api.ProxyInfo, string, error)
	// SwitchProxy selects a proxy inside a selector group.
	SwitchProxy(group, name string) error
	// Delay measures proxy latency in ms (URL-test against the ping URL).
	//
	// Deprecated: use DelayContext. Kept so existing callers and the legacy
	// Fyne targets compile unchanged; it delegates with a background context.
	Delay(proxyName string) (int64, error)
	// DelayContext measures latency under a caller-supplied context.
	//
	// The context is what lets a group test cancel every in-flight measurement
	// at once when the user stops the core, switches engine or starts a new run.
	// Without it each layer would invent its own unrelated deadline and the
	// cancellations could not reach the worker actually blocked on I/O.
	//
	// The per-node test budget (api.GetPingTestTimeoutMs) stays SEPARATE from
	// this context: the budget says "this node is too slow", the context says
	// "this run is over". Collapsing them would make a cancelled run
	// indistinguishable from a slow node.
	DelayContext(ctx context.Context, proxyName string) (int64, error)
}

// ClashTransport — классический транспорт поверх Clash HTTP API. Значения
// baseURL/token фиксируются на момент создания: caller (UI/APIService)
// строит транспорт непосредственно перед запросом, поэтому смена endpoint'а
// или override'а естественно даёт свежий транспорт.
type ClashTransport struct {
	BaseURL string
	Token   string
}

// NewClashTransport builds the classic Clash-HTTP transport.
func NewClashTransport(baseURL, token string) ClashTransport {
	return ClashTransport{BaseURL: baseURL, Token: token}
}

// GroupProxies implements ProxyTransport.
func (t ClashTransport) GroupProxies(group string) ([]api.ProxyInfo, string, error) {
	return api.GetProxiesInGroup(t.BaseURL, t.Token, group)
}

// SwitchProxy implements ProxyTransport.
func (t ClashTransport) SwitchProxy(group, name string) error {
	return api.SwitchProxy(t.BaseURL, t.Token, group, name)
}

// Delay implements ProxyTransport.
// Delay measures without a caller context: the legacy entry point.
func (t ClashTransport) Delay(proxyName string) (int64, error) {
	return t.DelayContext(context.Background(), proxyName)
}

// DelayContext measures through the Clash-compatible endpoint under ctx.
func (t ClashTransport) DelayContext(ctx context.Context, proxyName string) (int64, error) {
	return api.GetDelayContext(ctx, t.BaseURL, t.Token, proxyName)
}

// SetTransport устанавливает транспорт-override (daemon-режим). nil — вернуть
// классический Clash-путь.
func (apiSvc *APIService) SetTransport(t ProxyTransport) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.transportOverride = t
}

// TransportOverride возвращает установленный override (nil в classic-режиме).
// VerifiedClashEndpoint returns the Clash endpoint the launcher has PROVEN belongs to the
// backend's own core, for engines that establish one.
//
// The second return value distinguishes "this engine verified an endpoint" from "this engine
// has no verification concept". A caller that gets false must not conclude the endpoint is
// untrusted — a classic core has no second process to confuse it with — only that there is
// nothing engine-specific to prefer.
func (apiSvc *APIService) VerifiedClashEndpoint() (ClashTransport, bool) {
	apiSvc.StateMutex.RLock()
	provider := apiSvc.verifiedEndpoint
	apiSvc.StateMutex.RUnlock()
	if provider == nil {
		return ClashTransport{}, false
	}
	return provider(), true
}

// SetVerifiedClashEndpoint installs the engine's verified-endpoint provider.
//
// Called by the daemon engine once its Clash fallback can prove identity. The provider is
// consulted on each read rather than captured as a value, because verification EXPIRES: a
// captured endpoint would keep answering after its proof had gone stale.
func (apiSvc *APIService) SetVerifiedClashEndpoint(provider func() ClashTransport) {
	apiSvc.StateMutex.Lock()
	defer apiSvc.StateMutex.Unlock()
	apiSvc.verifiedEndpoint = provider
}

func (apiSvc *APIService) TransportOverride() ProxyTransport {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	return apiSvc.transportOverride
}

// SwitchProxyVia переключает прокси через ЯВНО переданный транспорт, минуя
// wireTransport. Нужно для UI-слоя Servers-tab, где источник транспорта —
// EffectiveProxyTransport (учитывает remote-override SPEC 064, который
// APIService не видит). Побочные эффекты (active name, last-selected,
// OnProxySwitched) те же, что у SwitchProxy.
func (apiSvc *APIService) SwitchProxyVia(t ProxyTransport, group, proxyName string) error {
	if err := t.SwitchProxy(group, proxyName); err != nil {
		return fmt.Errorf("failed to switch proxy: %w", err)
	}
	apiSvc.SetActiveProxyName(proxyName)
	apiSvc.SetLastSelectedProxyForGroup(group, proxyName)
	if apiSvc.OnProxySwitched != nil {
		apiSvc.OnProxySwitched()
	}
	return nil
}

// wireTransport resolves the transport APIService itself should use: the
// override when set (daemon mode), else Clash HTTP with the current
// config.json endpoint. Returns an error when neither is available.
func (apiSvc *APIService) wireTransport() (ProxyTransport, error) {
	apiSvc.StateMutex.RLock()
	defer apiSvc.StateMutex.RUnlock()
	if apiSvc.transportOverride != nil {
		return apiSvc.transportOverride, nil
	}
	if !apiSvc.Enabled {
		return nil, fmt.Errorf("clash_api is disabled")
	}
	return ClashTransport{BaseURL: apiSvc.BaseURL, Token: apiSvc.Token}, nil
}
