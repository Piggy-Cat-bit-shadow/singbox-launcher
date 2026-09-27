package config

import (
	"strings"
	"testing"

	"singbox-launcher/core/config/subscription"
)

// Ключи WireGuard — ровно 32 байта после декода base64. Значения
// детерминированные и не секретные: важна только длина.
const (
	wgValidPriv = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	wgValidPub  = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	wgShort     = "dG9vLXNob3J0" // 7 байт
)

func wgURI(priv, pub string) string {
	return "wireguard://" + priv + "@h:51820?publickey=" + pub + "&address=10.0.0.2%2F32"
}

// TestEndpointEmitRejectsInvalidKey — реестр объявляет для ключей wireguard
// `format: base64_32` с `on_invalid: drop_node` (код wg_key_invalid), но
// эмиттер endpoint'ов маршалил тело напрямую и правила значений не выполнял
// (в отличие от outbound'ов, идущих через materializeParsedNodeBody).
// Негодный ключ уезжал в config.json и ронял ВЕСЬ конфиг фаталом
// «decode private key: illegal base64 data» — то есть один мусорный узел
// оставлял человека без VPN (SPEC 145).
func TestEndpointEmitRejectsInvalidKey(t *testing.T) {
	tests := []struct {
		name    string
		priv    string
		pub     string
		wantErr bool
	}{
		{name: "valid 32-byte keys", priv: wgValidPriv, pub: wgValidPub, wantErr: false},
		{name: "not base64 private key", priv: "!!! not base64 !!!", pub: wgValidPub, wantErr: true},
		{name: "short private key", priv: wgShort, pub: wgValidPub, wantErr: true},
		{name: "not base64 public key", priv: wgValidPriv, pub: "!!! not base64 !!!", wantErr: true},
		{name: "short public key", priv: wgValidPriv, pub: wgShort, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, err := subscription.ParseNode(wgURI(tt.priv, tt.pub), nil)
			if err != nil {
				// Отказ на разборе — тоже приемлемый исход: узел не создан.
				if tt.wantErr {
					return
				}
				t.Fatalf("valid node failed to parse: %v", err)
			}
			_, endpointJSON, emitErr := EmitNodeJSONs(node)
			if tt.wantErr {
				if emitErr == nil {
					t.Fatalf("an invalid key must not reach config.json; got endpoint %d bytes", len(endpointJSON))
				}
				return
			}
			if emitErr != nil {
				t.Fatalf("valid node rejected: %v", emitErr)
			}
			if !strings.Contains(endpointJSON, wgValidPriv) {
				t.Fatalf("endpoint does not carry the key: %s", endpointJSON)
			}
		})
	}
}

// TestEndpointEmitKeepsValidNodeIntact — исправление не должно ломать
// корректный узел: тело эмитится, ключ и адрес на месте.
func TestEndpointEmitKeepsValidNodeIntact(t *testing.T) {
	node, err := subscription.ParseNode(wgURI(wgValidPriv, wgValidPub), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, endpointJSON, err := EmitNodeJSONs(node)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	for _, want := range []string{wgValidPriv, wgValidPub, "wireguard", "10.0.0.2/32"} {
		if !strings.Contains(endpointJSON, want) {
			t.Fatalf("endpoint lacks %q:\n%s", want, endpointJSON)
		}
	}
}

// TestMaterializeNodeBodyForVerdict — форма ввода проверяет узел тем же
// путём, что и сборка, поэтому негодное значение видно в форме, а не позже.
func TestMaterializeNodeBodyForVerdict(t *testing.T) {
	bad, err := subscription.ParseNode(wgURI(wgShort, wgValidPub), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, _, drop := MaterializeNodeBodyForVerdict(bad)
	if drop == nil {
		t.Fatal("an invalid key must be reported as a drop, so the form can reject it")
	}

	good, err := subscription.ParseNode(wgURI(wgValidPriv, wgValidPub), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	body, _, drop := MaterializeNodeBodyForVerdict(good)
	if drop != nil {
		t.Fatalf("valid node dropped: %v", drop)
	}
	if len(body) == 0 {
		t.Fatal("valid node produced an empty body")
	}
}
