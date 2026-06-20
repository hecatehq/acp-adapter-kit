package adaptertest_test

import (
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/adaptertest"
)

func TestAssertInitializeContract(t *testing.T) {
	t.Parallel()

	server := acp.NewServer(
		acp.AdapterInfo{
			Name:    "test-adapter",
			Title:   "Test Adapter",
			Version: "1.2.3",
			Capabilities: acp.Capabilities{
				Images:          true,
				EmbeddedContext: true,
				MCPHTTP:         true,
				MCPSSE:          true,
				LoadSession:     true,
			},
		},
		acp.WithAuthLogout(),
		acp.WithAuthMethods([]acp.AuthMethod{{
			ID:   "agent-login",
			Name: "Agent login",
		}}),
	)
	adaptertest.AssertInitializeContract(t, server, adaptertest.InitializeContract{
		Name:            "test-adapter",
		Title:           "Test Adapter",
		Version:         "1.2.3",
		Images:          true,
		EmbeddedContext: true,
		MCPHTTP:         true,
		MCPSSE:          true,
		LoadSession:     true,
		Logout:          true,
		AuthMethodIDs:   []string{"agent-login"},
	})
}
