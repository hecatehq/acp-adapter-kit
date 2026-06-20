package adaptertest

import (
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
)

type ConfigOptionContract struct {
	ID           string
	Category     string
	CurrentValue string
}

type SessionBootstrapContract struct {
	CWD               string
	ConfigOptions     []ConfigOptionContract
	AvailableCommands []string
}

type SessionBootstrapResult struct {
	SessionID string
}

// AssertSessionBootstrapContract verifies the `session/new` controls that
// Hecate renders for External Agent chats: config selectors in the response and
// slash commands in the preceding availability notification.
func AssertSessionBootstrapContract(t testing.TB, server *acp.Server, want SessionBootstrapContract) SessionBootstrapResult {
	t.Helper()
	if server == nil {
		t.Fatal("server is nil")
	}
	if want.CWD == "" {
		want.CWD = t.TempDir()
	}
	client := acptest.NewClient(t, server)
	responses := client.Send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "session/new",
		"params":  map[string]any{"cwd": want.CWD},
	})
	if len(responses) == 0 {
		t.Fatal("session/new produced no responses")
	}
	resultResp := responses[len(responses)-1]
	var result struct {
		SessionID     string `json:"sessionId"`
		ConfigOptions []struct {
			ID           string `json:"id"`
			Category     string `json:"category"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	resultResp.ResultInto(t, &result)
	if result.SessionID == "" {
		t.Fatal("sessionId is empty")
	}
	gotOptions := make([]ConfigOptionContract, 0, len(result.ConfigOptions))
	for _, option := range result.ConfigOptions {
		gotOptions = append(gotOptions, ConfigOptionContract{
			ID:           option.ID,
			Category:     option.Category,
			CurrentValue: option.CurrentValue,
		})
	}
	if !equalConfigOptions(gotOptions, want.ConfigOptions) {
		t.Fatalf("config options = %#v, want %#v", gotOptions, want.ConfigOptions)
	}

	gotCommands := availableCommandNames(t, responses[:len(responses)-1])
	if !equalStrings(gotCommands, want.AvailableCommands) {
		t.Fatalf("available commands = %#v, want %#v", gotCommands, want.AvailableCommands)
	}
	return SessionBootstrapResult{SessionID: result.SessionID}
}

func availableCommandNames(t testing.TB, responses []acptest.Response) []string {
	t.Helper()
	for _, response := range responses {
		var notification struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate     string `json:"sessionUpdate"`
				AvailableCommands []struct {
					Name string `json:"name"`
				} `json:"availableCommands"`
			} `json:"update"`
		}
		response.ParamsInto(t, &notification)
		if notification.Update.SessionUpdate != "available_commands_update" {
			continue
		}
		out := make([]string, 0, len(notification.Update.AvailableCommands))
		for _, command := range notification.Update.AvailableCommands {
			out = append(out, command.Name)
		}
		return out
	}
	return nil
}

func equalConfigOptions(a, b []ConfigOptionContract) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
