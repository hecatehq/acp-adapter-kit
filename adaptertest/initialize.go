// Package adaptertest provides reusable ACP adapter conformance assertions for
// adapter repositories. It intentionally checks provider-neutral contracts only;
// provider command argv, env allowlists, and stream mappings stay in each
// adapter repo.
package adaptertest

import (
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/acptest"
)

type InitializeContract struct {
	Name            string
	Title           string
	Version         string
	Images          bool
	EmbeddedContext bool
	MCPHTTP         bool
	MCPSSE          bool
	LoadSession     bool
	Logout          bool
	AuthMethodIDs   []string
}

// AssertInitializeContract verifies the initialize surface Hecate relies on
// when deciding adapter capabilities and local auth actions.
func AssertInitializeContract(t testing.TB, server *acp.Server, want InitializeContract) {
	t.Helper()
	if server == nil {
		t.Fatal("server is nil")
	}
	client := acptest.NewClient(t, server)
	resp := client.Request("initialize", map[string]any{})
	var result struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
			Auth *struct {
				Logout map[string]any `json:"logout"`
			} `json:"auth"`
		} `json:"agentCapabilities"`
		AgentInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AuthMethods []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"authMethods"`
	}
	resp.ResultInto(t, &result)
	if result.ProtocolVersion != 1 {
		t.Fatalf("protocolVersion = %d, want 1", result.ProtocolVersion)
	}
	if result.AgentInfo.Name != want.Name || result.AgentInfo.Title != want.Title || result.AgentInfo.Version != want.Version {
		t.Fatalf("agentInfo = %#v, want name=%q title=%q version=%q", result.AgentInfo, want.Name, want.Title, want.Version)
	}
	caps := result.AgentCapabilities
	if caps.LoadSession != want.LoadSession {
		t.Fatalf("loadSession = %v, want %v", caps.LoadSession, want.LoadSession)
	}
	if caps.PromptCapabilities.Image != want.Images {
		t.Fatalf("promptCapabilities.image = %v, want %v", caps.PromptCapabilities.Image, want.Images)
	}
	if caps.PromptCapabilities.EmbeddedContext != want.EmbeddedContext {
		t.Fatalf("promptCapabilities.embeddedContext = %v, want %v", caps.PromptCapabilities.EmbeddedContext, want.EmbeddedContext)
	}
	if caps.MCPCapabilities.HTTP != want.MCPHTTP {
		t.Fatalf("mcpCapabilities.http = %v, want %v", caps.MCPCapabilities.HTTP, want.MCPHTTP)
	}
	if caps.MCPCapabilities.SSE != want.MCPSSE {
		t.Fatalf("mcpCapabilities.sse = %v, want %v", caps.MCPCapabilities.SSE, want.MCPSSE)
	}
	gotLogout := caps.Auth != nil && caps.Auth.Logout != nil
	if gotLogout != want.Logout {
		t.Fatalf("auth.logout advertised = %v, want %v", gotLogout, want.Logout)
	}
	gotMethods := make([]string, 0, len(result.AuthMethods))
	for _, method := range result.AuthMethods {
		gotMethods = append(gotMethods, method.ID)
		if method.ID == "" {
			t.Fatalf("auth method = %#v, want non-empty id", method)
		}
	}
	if !equalStrings(gotMethods, want.AuthMethodIDs) {
		t.Fatalf("auth method ids = %#v, want %#v", gotMethods, want.AuthMethodIDs)
	}
}

func equalStrings(a, b []string) bool {
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
