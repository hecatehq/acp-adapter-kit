package runtimeacp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestSetConfigOptionReturnsRawConfigState(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"configOptions":[{"id":"model","currentValue":"smart"}]}`)}

	raw, err := runtimeacp.SetConfigOption(context.Background(), client, runtimeacp.SetConfigOptionParams{
		SessionID: "sess-test",
		ConfigID:  "model",
		Value:     "smart",
	})
	if err != nil {
		t.Fatalf("SetConfigOption returned error: %v", err)
	}
	if client.method != "session/set_config_option" {
		t.Fatalf("method = %q, want session/set_config_option", client.method)
	}
	var params runtimeacp.SetConfigOptionParams
	mustJSONRoundTrip(t, client.params, &params)
	if params.SessionID != "sess-test" || params.ConfigID != "model" || params.Value != "smart" {
		t.Fatalf("params = %#v, want model smart", params)
	}
	if !strings.Contains(string(raw), `"configOptions"`) {
		t.Fatalf("raw result = %s, want configOptions", raw)
	}
}

func TestSetConfigOptionRawPreservesUnknownParams(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"configOptions":[]}`)}

	_, err := runtimeacp.SetConfigOptionRaw(context.Background(), client, json.RawMessage(`{"sessionId":"sess-test","configId":"model","value":"smart","x-extra":1}`))
	if err != nil {
		t.Fatalf("SetConfigOptionRaw returned error: %v", err)
	}
	if client.method != "session/set_config_option" {
		t.Fatalf("method = %q, want session/set_config_option", client.method)
	}
	var params map[string]json.RawMessage
	mustJSONRoundTrip(t, client.params, &params)
	if string(params["x-extra"]) != `1` {
		t.Fatalf("x-extra = %s, want 1", params["x-extra"])
	}
}

func TestSetModeReturnsRawModeState(t *testing.T) {
	client := &recordingACPClient{result: json.RawMessage(`{"modes":{"currentModeId":"code"}}`)}

	raw, err := runtimeacp.SetMode(context.Background(), client, runtimeacp.SetModeParams{
		SessionID: "sess-test",
		ModeID:    "code",
	})
	if err != nil {
		t.Fatalf("SetMode returned error: %v", err)
	}
	if client.method != "session/set_mode" {
		t.Fatalf("method = %q, want session/set_mode", client.method)
	}
	if !strings.Contains(string(raw), `"modes"`) {
		t.Fatalf("raw result = %s, want modes", raw)
	}
}
