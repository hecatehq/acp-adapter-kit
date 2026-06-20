package adaptertest_test

import (
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/adaptertest"
	"github.com/hecatehq/acp-adapter-kit/commandbridge"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestAssertSessionBootstrapContract(t *testing.T) {
	t.Parallel()

	bridge := commandbridge.New(commandbridge.Spec{
		NewID: func() string { return "session-test" },
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			DefaultValue: "default",
			Options:      []commandbridge.SelectValue{{Value: "default", Name: "Default"}},
		}},
		Commands: []commandbridge.AvailableCommand{
			{Name: "review", Description: "Review changes"},
			{Name: "init", Description: "Initialize instructions"},
		},
		BuildPrompt: func(commandbridge.Session, runtimeacp.PromptParams) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{}, nil
		},
	})
	result := adaptertest.AssertSessionBootstrapContract(t, acp.NewServer(
		acp.AdapterInfo{Name: "test-adapter"},
		bridge.Options()...,
	), adaptertest.SessionBootstrapContract{
		CWD: "/tmp/work",
		ConfigOptions: []adaptertest.ConfigOptionContract{{
			ID:           "model",
			Category:     "model",
			CurrentValue: "default",
		}},
		AvailableCommands: []string{"review", "init"},
	})
	if result.SessionID != "session-test" {
		t.Fatalf("SessionID = %q, want session-test", result.SessionID)
	}
}
