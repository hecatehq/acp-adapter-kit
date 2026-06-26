package runtimeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type TerminalCreateParams struct {
	SessionID       string   `json:"sessionId"`
	Command         string   `json:"command"`
	Args            []string `json:"args,omitempty"`
	CWD             string   `json:"cwd,omitempty"`
	OutputByteLimit int      `json:"outputByteLimit,omitempty"`
}

type TerminalCreateResult struct {
	TerminalID string `json:"terminalId"`

	raw json.RawMessage
}

type TerminalOutputParams struct {
	SessionID  string `json:"sessionId,omitempty"`
	TerminalID string `json:"terminalId"`
}

type TerminalOutputResult struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated,omitempty"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`

	raw json.RawMessage
}

type TerminalExitStatus struct {
	ExitCode *int `json:"exitCode,omitempty"`
}

type TerminalWaitForExitParams struct {
	SessionID  string `json:"sessionId,omitempty"`
	TerminalID string `json:"terminalId"`
}

type TerminalWaitForExitResult struct {
	ExitCode *int `json:"exitCode,omitempty"`

	raw json.RawMessage
}

type TerminalKillParams struct {
	SessionID  string `json:"sessionId,omitempty"`
	TerminalID string `json:"terminalId"`
}

type TerminalReleaseParams struct {
	SessionID  string `json:"sessionId,omitempty"`
	TerminalID string `json:"terminalId"`
}

func TerminalCreate(ctx context.Context, client JSONRPCClient, params TerminalCreateParams) (TerminalCreateResult, error) {
	resultData, err := requestRaw(ctx, client, "terminal/create", params)
	if err != nil {
		return TerminalCreateResult{}, err
	}
	var result TerminalCreateResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return TerminalCreateResult{}, fmt.Errorf("decode terminal/create result: %w", err)
	}
	if result.TerminalID == "" {
		return TerminalCreateResult{}, errors.New("terminal/create result missing terminalId")
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r TerminalCreateResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias TerminalCreateResult
	return json.Marshal(alias(r))
}

func TerminalOutput(ctx context.Context, client JSONRPCClient, params TerminalOutputParams) (TerminalOutputResult, error) {
	resultData, err := requestRaw(ctx, client, "terminal/output", params)
	if err != nil {
		return TerminalOutputResult{}, err
	}
	var result TerminalOutputResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return TerminalOutputResult{}, fmt.Errorf("decode terminal/output result: %w", err)
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r TerminalOutputResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias TerminalOutputResult
	return json.Marshal(alias(r))
}

func TerminalWaitForExit(ctx context.Context, client JSONRPCClient, params TerminalWaitForExitParams) (TerminalWaitForExitResult, error) {
	resultData, err := requestRaw(ctx, client, "terminal/wait_for_exit", params)
	if err != nil {
		return TerminalWaitForExitResult{}, err
	}
	var result TerminalWaitForExitResult
	if err := json.Unmarshal(resultData, &result); err != nil {
		return TerminalWaitForExitResult{}, fmt.Errorf("decode terminal/wait_for_exit result: %w", err)
	}
	result.raw = append(json.RawMessage(nil), resultData...)
	return result, nil
}

func (r TerminalWaitForExitResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) != 0 {
		return append([]byte(nil), r.raw...), nil
	}
	type alias TerminalWaitForExitResult
	return json.Marshal(alias(r))
}

func TerminalKill(ctx context.Context, client JSONRPCClient, params TerminalKillParams) error {
	var result map[string]any
	return requestInto(ctx, client, "terminal/kill", params, &result)
}

func TerminalRelease(ctx context.Context, client JSONRPCClient, params TerminalReleaseParams) error {
	var result map[string]any
	return requestInto(ctx, client, "terminal/release", params, &result)
}
