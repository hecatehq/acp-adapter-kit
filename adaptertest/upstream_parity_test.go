package adaptertest

import (
	"context"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/commandbridge"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestAssertUpstreamParityContract(t *testing.T) {
	AssertUpstreamParityContract(t, parityServer(false), UpstreamParityContract{
		CWD:          t.TempDir(),
		AuthMethodID: "agent-login",
		ConfigChange: ConfigChangeContract{
			ID:    "model",
			Value: "smart",
		},
		LoadUnknownSession: LoadUnknownSessionContract{
			SessionID: "missing-session",
			CWD:       t.TempDir(),
			Allowed:   false,
		},
	})
}

func TestAssertUpstreamParityContractAdoptsUnknownSessions(t *testing.T) {
	AssertUpstreamParityContract(t, parityServer(true), UpstreamParityContract{
		CWD:          t.TempDir(),
		AuthMethodID: "agent-login",
		ConfigChange: ConfigChangeContract{
			ID:    "model",
			Value: "smart",
		},
		LoadUnknownSession: LoadUnknownSessionContract{
			SessionID: "known-upstream-session",
			CWD:       t.TempDir(),
			Allowed:   true,
		},
	})
}

func parityServer(loadUnknownSessions bool) *acp.Server {
	bridge := commandbridge.New(commandbridge.Spec{
		LoadUnknownSessions: loadUnknownSessions,
		AuthMethods: []acp.AuthMethod{{
			ID:   "agent-login",
			Name: "Agent login",
		}},
		Options: []commandbridge.SelectConfigOption{{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			DefaultValue: "default",
			Options: []commandbridge.SelectValue{
				{Value: "default", Name: "Default"},
				{Value: "smart", Name: "Smart"},
			},
		}},
		Commands: []commandbridge.AvailableCommand{{
			Name:      "review",
			InputHint: "optional focus",
		}},
		BuildAuthenticate: func(string) (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent", Args: []string{"login"}}, nil
		},
		BuildLogout: func() (adapterprocess.Spec, error) {
			return adapterprocess.Spec{Command: "agent", Args: []string{"logout"}}, nil
		},
		Runner: commandbridge.RunnerFunc(func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error) {
			return adapterprocess.Result{}, nil
		}),
	})
	return acp.NewServer(acp.AdapterInfo{
		Name:    "parity-agent",
		Title:   "Parity Agent",
		Version: "test",
		Capabilities: acp.Capabilities{
			LoadSession: true,
		},
	}, bridge.Options()...)
}
