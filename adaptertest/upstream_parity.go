package adaptertest

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
)

type UpstreamParityContract struct {
	CWD                string
	AuthMethodID       string
	ConfigChange       ConfigChangeContract
	LoadUnknownSession LoadUnknownSessionContract
}

type ConfigChangeContract struct {
	ID    string
	Value string
}

type LoadUnknownSessionContract struct {
	SessionID string
	CWD       string
	Allowed   bool
}

// AssertUpstreamParityContract verifies portable behaviors adapted from prior
// adapter suites: auth method gating, session bootstrap, config mutation shape,
// session list/load, cancel/close/delete, and unknown session load behavior.
// Provider-internal SDK tests stay in adapter repos.
func AssertUpstreamParityContract(t testing.TB, server *acp.Server, want UpstreamParityContract) {
	t.Helper()
	if server == nil {
		t.Fatal("server is nil")
	}
	if want.CWD == "" {
		want.CWD = t.TempDir()
	}

	client := acptest.NewClient(t, server)
	created := assertSessionNewParity(t, client, want.CWD)
	sessionID := created.SessionID
	if sessionID == "" {
		t.Fatal("session/new sessionId is empty")
	}
	assertUnsupportedAuthMethod(t, client, want.AuthMethodID)
	if want.ConfigChange.ID != "" {
		assertConfigChangeParity(t, client, sessionID, want.ConfigChange)
	}
	assertSessionListContains(t, client, sessionID, want.CWD)
	assertSessionLoadExisting(t, client, sessionID)
	assertUnknownSessionLoadBehavior(t, client, want.LoadUnknownSession)
	assertSessionCancelCloseDelete(t, client, sessionID, !want.LoadUnknownSession.Allowed)
}

type paritySession struct {
	SessionID     string               `json:"sessionId"`
	ConfigOptions []parityConfigOption `json:"configOptions"`
}

type parityConfigOption struct {
	ID           string `json:"id"`
	Category     string `json:"category"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"options"`
}

func assertSessionNewParity(t testing.TB, client *acptest.Client, cwd string) paritySession {
	t.Helper()
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "session/new",
		"params":  map[string]any{"cwd": cwd},
	})
	if len(responses) < 1 {
		t.Fatal("session/new produced no responses")
	}
	var sawCommands bool
	for _, response := range responses[:len(responses)-1] {
		if response.Method != "session/update" {
			continue
		}
		var update struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate     string `json:"sessionUpdate"`
				AvailableCommands []struct {
					Name string `json:"name"`
				} `json:"availableCommands"`
			} `json:"update"`
		}
		response.ParamsInto(t, &update)
		if update.SessionID != "" && update.Update.SessionUpdate == "available_commands_update" {
			if len(update.Update.AvailableCommands) == 0 {
				t.Fatalf("available_commands_update = %#v, want at least one command", update)
			}
			sawCommands = true
		}
	}
	if !sawCommands {
		t.Fatalf("session/new responses = %#v, want available_commands_update before result", responses)
	}
	var created paritySession
	responses[len(responses)-1].ResultInto(t, &created)
	if len(created.ConfigOptions) == 0 {
		t.Fatalf("session/new result = %#v, want configOptions", created)
	}
	for _, option := range created.ConfigOptions {
		if option.ID == "" || option.CurrentValue == "" || len(option.Options) == 0 {
			t.Fatalf("config option = %#v, want id/currentValue/options", option)
		}
	}
	return created
}

func assertUnsupportedAuthMethod(t testing.TB, client *acptest.Client, allowed string) {
	t.Helper()
	unsupported := "upstream-parity-unsupported-login"
	if unsupported == allowed {
		unsupported = "upstream-parity-other-login"
	}
	resp := client.Request("authenticate", map[string]any{"methodId": unsupported})
	if resp.Error == nil {
		t.Fatalf("authenticate(%q) error = nil, want invalid params", unsupported)
	}
	if resp.Error.Code != -32602 {
		t.Fatalf("authenticate(%q) error = %+v, want invalid params code", unsupported, *resp.Error)
	}
}

func assertConfigChangeParity(t testing.TB, client *acptest.Client, sessionID string, change ConfigChangeContract) {
	t.Helper()
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/set_config_option",
		"params": map[string]any{
			"sessionId": sessionID,
			"configId":  change.ID,
			"value":     change.Value,
		},
	})
	if len(responses) != 2 {
		t.Fatalf("set_config_option responses = %#v, want config update notification + result", responses)
	}
	update := configUpdatePayload(t, responses[0])
	if optionCurrentValue(update.ConfigOptions, change.ID) != change.Value {
		t.Fatalf("config update options = %#v, want %s=%q", update.ConfigOptions, change.ID, change.Value)
	}
	var result struct {
		ConfigOptions []parityConfigOption `json:"configOptions"`
	}
	responses[1].ResultInto(t, &result)
	if optionCurrentValue(result.ConfigOptions, change.ID) != change.Value {
		t.Fatalf("set_config_option result = %#v, want %s=%q", result.ConfigOptions, change.ID, change.Value)
	}
	invalid := client.Request("session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  change.ID,
		"value":     "upstream-parity-invalid-value",
	})
	if invalid.Error == nil || invalid.Error.Code != -32602 {
		t.Fatalf("invalid set_config_option error = %+v, want invalid params", invalid.Error)
	}
	missing := client.Request("session/set_config_option", map[string]any{
		"sessionId": "upstream-parity-missing-session",
		"configId":  change.ID,
		"value":     change.Value,
	})
	if missing.Error == nil {
		t.Fatal("set_config_option missing session error = nil, want not found")
	}
}

func configUpdatePayload(t testing.TB, response acptest.Response) struct {
	SessionID     string               `json:"sessionId"`
	ConfigOptions []parityConfigOption `json:"configOptions"`
} {
	t.Helper()
	if response.Method != "session/update" {
		t.Fatalf("response method = %q, want session/update", response.Method)
	}
	var payload struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string               `json:"sessionUpdate"`
			ConfigOptions []parityConfigOption `json:"configOptions"`
		} `json:"update"`
	}
	response.ParamsInto(t, &payload)
	if payload.Update.SessionUpdate != "config_option_update" {
		t.Fatalf("session update = %#v, want config_option_update", payload.Update)
	}
	return struct {
		SessionID     string               `json:"sessionId"`
		ConfigOptions []parityConfigOption `json:"configOptions"`
	}{SessionID: payload.SessionID, ConfigOptions: payload.Update.ConfigOptions}
}

func optionCurrentValue(options []parityConfigOption, id string) string {
	for _, option := range options {
		if option.ID == id {
			return option.CurrentValue
		}
	}
	return ""
}

func assertSessionListContains(t testing.TB, client *acptest.Client, sessionID, cwd string) {
	t.Helper()
	resp := client.Request("session/list", map[string]any{})
	var result struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			CWD       string `json:"cwd"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"sessions"`
	}
	resp.ResultInto(t, &result)
	sort.Slice(result.Sessions, func(i, j int) bool { return result.Sessions[i].SessionID < result.Sessions[j].SessionID })
	for _, session := range result.Sessions {
		if session.SessionID == sessionID {
			if session.CWD != cwd || session.UpdatedAt == "" {
				t.Fatalf("listed session = %#v, want cwd=%q and updatedAt", session, cwd)
			}
			return
		}
	}
	t.Fatalf("session/list = %#v, want session %q", result.Sessions, sessionID)
}

func assertSessionLoadExisting(t testing.TB, client *acptest.Client, sessionID string) {
	t.Helper()
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/load",
		"params":  map[string]any{"sessionId": sessionID},
	})
	if len(responses) < 1 {
		t.Fatal("session/load produced no responses")
	}
	var result struct {
		ConfigOptions []parityConfigOption `json:"configOptions"`
	}
	responses[len(responses)-1].ResultInto(t, &result)
	if len(result.ConfigOptions) == 0 {
		t.Fatalf("session/load result = %#v, want configOptions", result)
	}
}

func assertUnknownSessionLoadBehavior(t testing.TB, client *acptest.Client, want LoadUnknownSessionContract) {
	t.Helper()
	if want.SessionID == "" {
		return
	}
	cwd := want.CWD
	if cwd == "" {
		cwd = t.TempDir()
	}
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "session/load",
		"params": map[string]any{
			"sessionId": want.SessionID,
			"cwd":       cwd,
		},
	})
	if len(responses) == 0 {
		t.Fatal("unknown session/load produced no responses")
	}
	final := responses[len(responses)-1]
	if want.Allowed {
		if final.Error != nil {
			t.Fatalf("unknown session/load error = %+v, want adopted session", *final.Error)
		}
		var result struct {
			ConfigOptions []parityConfigOption `json:"configOptions"`
		}
		final.ResultInto(t, &result)
		if len(result.ConfigOptions) == 0 {
			t.Fatalf("unknown session/load result = %#v, want configOptions", result)
		}
		assertSessionListContains(t, client, want.SessionID, cwd)
		return
	}
	if final.Error == nil {
		raw, _ := json.Marshal(final.Result)
		t.Fatalf("unknown session/load result = %s, want not found error", raw)
	}
}

func assertSessionCancelCloseDelete(t testing.TB, client *acptest.Client, sessionID string, expectLoadAfterDeleteNotFound bool) {
	t.Helper()
	cancel := client.Request("session/cancel", map[string]any{"sessionId": sessionID})
	var cancelResult struct {
		Cancelled bool `json:"cancelled"`
	}
	cancel.ResultInto(t, &cancelResult)
	if cancelResult.Cancelled {
		t.Fatalf("session/cancel returned cancelled=true for idle session %q", sessionID)
	}
	closeResp := client.Request("session/close", map[string]any{"sessionId": sessionID})
	var closeResult map[string]any
	closeResp.ResultInto(t, &closeResult)
	if len(closeResult) != 0 {
		t.Fatalf("session/close result = %#v, want empty object", closeResult)
	}
	deleteResp := client.Request("session/delete", map[string]any{"sessionId": sessionID})
	var deleteResult map[string]any
	deleteResp.ResultInto(t, &deleteResult)
	if len(deleteResult) != 0 {
		t.Fatalf("session/delete result = %#v, want empty object", deleteResult)
	}
	if !expectLoadAfterDeleteNotFound {
		return
	}
	loadAfterDelete := client.Request("session/load", map[string]any{"sessionId": sessionID})
	if loadAfterDelete.Error == nil {
		t.Fatalf("session/load after delete error = nil, want not found")
	}
}
