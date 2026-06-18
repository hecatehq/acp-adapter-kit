package runtimehost

import (
	"context"
	"fmt"

	"github.com/hecatehq/acp-adapter-kit/acp"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
	"github.com/hecatehq/acp-adapter-kit/runtimebridge"
	"github.com/hecatehq/acp-adapter-kit/runtimejsonrpc"
	"github.com/hecatehq/acp-adapter-kit/runtimeproc"
)

type Spec struct {
	Launcher           runtimeproc.Launcher
	Launch             runtimeproc.LaunchSpec
	ClientInfo         runtimeacp.ImplementationInfo
	ClientCapabilities runtimeacp.ClientCapabilities
	MaxMessageBytes    int
	EventBuffer        int
}

type Host struct {
	client           *runtimejsonrpc.Client
	initializeResult runtimeacp.InitializeResult
	bridge           *runtimebridge.Bridge
}

func Start(ctx context.Context, spec Spec) (*Host, error) {
	client, err := runtimejsonrpc.Connect(ctx, runtimejsonrpc.ConnectSpec{
		Launcher:        spec.Launcher,
		Launch:          spec.Launch,
		MaxMessageBytes: spec.MaxMessageBytes,
		EventBuffer:     spec.EventBuffer,
	})
	if err != nil {
		return nil, err
	}

	result, err := runtimeacp.Initialize(ctx, client, runtimeacp.InitializeParams{
		ProtocolVersion:    runtimeacp.ProtocolVersion,
		ClientInfo:         spec.ClientInfo,
		ClientCapabilities: spec.ClientCapabilities,
	})
	if err != nil {
		closeClient(client)
		return nil, fmt.Errorf("initialize runtime ACP: %w", err)
	}

	return &Host{
		client:           client,
		initializeResult: result,
		bridge:           runtimebridge.New(client),
	}, nil
}

func (h *Host) InitializeResult() runtimeacp.InitializeResult {
	if h == nil {
		return runtimeacp.InitializeResult{}
	}
	return h.initializeResult
}

func (h *Host) Options() []acp.Option {
	if h == nil || h.bridge == nil {
		return nil
	}
	return h.bridge.Options()
}

func (h *Host) RuntimeClient() runtimebridge.RuntimeClient {
	if h == nil {
		return nil
	}
	return h.client
}

func (h *Host) Kill() error {
	if h == nil || h.client == nil {
		return nil
	}
	return h.client.Kill()
}

func (h *Host) Wait() error {
	if h == nil || h.client == nil {
		return nil
	}
	return h.client.Wait()
}

func (h *Host) Close() error {
	if h == nil || h.client == nil {
		return nil
	}
	return closeClient(h.client)
}

func closeClient(client *runtimejsonrpc.Client) error {
	if client == nil {
		return nil
	}
	if err := client.Kill(); err != nil {
		return err
	}
	_ = client.Wait()
	return nil
}
