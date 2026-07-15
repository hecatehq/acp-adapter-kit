package commandbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hecatehq/acp-adapter-kit/acp"
	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

type Runner interface {
	Run(context.Context, adapterprocess.Spec) (adapterprocess.Result, error)
}

type StreamRunner interface {
	RunStream(context.Context, adapterprocess.Spec, func([]byte) error) (adapterprocess.Result, error)
}

type RunnerFunc func(context.Context, adapterprocess.Spec) (adapterprocess.Result, error)

func (f RunnerFunc) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	return f(ctx, spec)
}

type ProcessRunner struct {
	baseEnv    []string
	baseEnvSet bool
}

// NewProcessRunner binds provider subprocesses to a host-supplied environment.
// The slice is copied so later caller mutation cannot widen the runtime
// boundary. Passing an empty slice intentionally inherits nothing.
func NewProcessRunner(baseEnv []string) ProcessRunner {
	if baseEnv == nil {
		baseEnv = []string{}
	}
	return ProcessRunner{
		baseEnv:    append([]string(nil), baseEnv...),
		baseEnvSet: true,
	}
}

func (r ProcessRunner) Run(ctx context.Context, spec adapterprocess.Spec) (adapterprocess.Result, error) {
	if !r.baseEnvSet {
		return adapterprocess.Run(ctx, spec)
	}
	return adapterprocess.RunWithBaseEnv(ctx, spec, r.baseEnv)
}

func (r ProcessRunner) RunStream(ctx context.Context, spec adapterprocess.Spec, onStdout func([]byte) error) (adapterprocess.Result, error) {
	if !r.baseEnvSet {
		return adapterprocess.RunStream(ctx, spec, onStdout)
	}
	return adapterprocess.RunStreamWithBaseEnv(ctx, spec, r.baseEnv, onStdout)
}

type PromptCommandBuilder func(Session, runtimeacp.PromptParams) (adapterprocess.Spec, error)

type AuthenticateCommandBuilder func(methodID string) (adapterprocess.Spec, error)

type LogoutCommandBuilder func() (adapterprocess.Spec, error)

type AuthRequiredDetector func(adapterprocess.Result, error) bool

type PromptFailureKind uint8

const (
	PromptFailureUnknown PromptFailureKind = iota
	PromptFailureNativeSessionMissing
)

// PromptFailureClassifier maps a provider-specific non-zero command exit to a
// provider-neutral wire discriminator. The bridge invokes it only for a
// process.ExitError; classifiers must additionally fail closed on partial or
// truncated output and prove provider-specific identity/session invariants.
// The bridge reports the classification without retrying or mutating session
// state; the host remains responsible for deciding whether replacing native
// state is safe for its persisted session.
type PromptFailureClassifier func(Session, adapterprocess.Spec, adapterprocess.Result, error) PromptFailureKind

type Spec struct {
	Runner Runner
	// NewID overrides the default cryptographically random ACP session id.
	// Custom generators must return a non-empty id that remains unique across
	// adapter process restarts.
	NewID                  func() string
	LoadUnknownSessions    bool
	AuthMethods            []acp.AuthMethod
	Options                []SelectConfigOption
	Commands               []AvailableCommand
	IncludeTranscript      bool
	MaxTranscriptExchanges int
	BuildPrompt            PromptCommandBuilder
	BuildAuthenticate      AuthenticateCommandBuilder
	BuildLogout            LogoutCommandBuilder
	AuthRequired           AuthRequiredDetector
	ClassifyPromptFailure  PromptFailureClassifier
	NewStreamParser        func(Session, runtimeacp.PromptParams) StreamParser
	Now                    func() time.Time
	// PromptResourceLimits bounds prompt-scoped local file preparation.
	PromptResourceLimits PromptResourceLimits
	// PromptResourceTempDir selects the parent for private prompt directories.
	// It must be an absolute trusted local directory: canonical ancestors must
	// satisfy the platform ownership, ACL, and non-reparse rules. The
	// operating-system temporary directory is used when it is empty.
	PromptResourceTempDir string
}

type Bridge struct {
	spec Spec

	nextID atomic.Int64

	mu       sync.Mutex
	sessions map[string]*sessionState
	active   map[string]*activePrompt

	promptResourceCleanupHook func(string) error
	promptMethodContext       func(*acp.MethodContext) context.Context
	promptStagePrepared       func(*promptResourceStage)
}

type activePrompt struct {
	cancel    context.CancelFunc
	finalized bool
}

type Session struct {
	ID                    string
	CWD                   string
	AdditionalDirectories []string
	MCPServers            []runtimeacp.MCPServer
	Config                map[string]string
	ModeID                string
	Title                 string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	// PromptCount counts successful prompt commands run for this ACP session ID.
	PromptCount int
	// Adopted is true when the bridge adopted a host-known session ID via load/resume.
	Adopted bool
}

type SelectConfigOption struct {
	ID           string
	Name         string
	Description  string
	Category     string
	DefaultValue string
	Options      []SelectValue
}

type SelectValue struct {
	Value       string
	Name        string
	Description string
}

type AvailableCommand struct {
	Name        string
	Description string
	InputHint   string
}

type sessionState struct {
	Session
	transcript []transcriptExchange
}

type transcriptExchange struct {
	User      string
	Assistant string
}

type sessionInfo struct {
	Title     string
	UpdatedAt time.Time
}

const (
	defaultMaxTranscriptExchanges = 8
	toolOutputPreviewLimit        = 8 * 1024
)

func New(spec Spec) *Bridge {
	if spec.Runner == nil {
		spec.Runner = ProcessRunner{}
	}
	return &Bridge{
		spec:     spec,
		sessions: map[string]*sessionState{},
		active:   map[string]*activePrompt{},
	}
}

func (b *Bridge) Options() []acp.Option {
	if b == nil {
		return nil
	}
	options := []acp.Option{
		acp.WithMethod("session/new", b.newSession),
		acp.WithMethod("session/fork", b.forkSession),
		acp.WithMethod("session/load", b.loadSession),
		acp.WithMethod("session/resume", b.resumeSession),
		acp.WithMethod("session/list", b.listSessions),
		acp.WithMethod("session/set_config_option", b.setConfigOption),
		acp.WithMethod("session/set_mode", b.setMode),
		acp.WithMethod("session/prompt", b.prompt),
		acp.WithConcurrentMethod("session/cancel", b.cancelMethod),
		acp.WithConcurrentMethod("session/close", b.closeSession),
		acp.WithMethod("session/delete", b.deleteSession),
		acp.WithNotification("session/cancel", b.cancelNotification),
	}
	if b.spec.BuildLogout != nil {
		options = append(options, acp.WithAuthLogout())
		options = append(options, acp.WithMethod("logout", b.logout))
	}
	if b.spec.BuildAuthenticate != nil {
		options = append(options, acp.WithAuthMethods(b.spec.AuthMethods))
		options = append(options, acp.WithMethod("authenticate", b.authenticate))
	}
	return options
}

func (b *Bridge) authenticate(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.AuthenticateParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	methodID := strings.TrimSpace(req.MethodID)
	if methodID == "" {
		return nil, invalidParams("methodId is required", nil)
	}
	if !b.authMethodAllowed(methodID) {
		return nil, invalidParams("unknown auth method", methodID)
	}
	command, err := b.spec.BuildAuthenticate(methodID)
	if err != nil {
		return nil, invalidParams("build authenticate command", err.Error())
	}
	result, err := b.spec.Runner.Run(methodContext(ctx), command)
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "authenticate command failed", Data: commandErrorData(result, err)}
	}
	return map[string]any{}, nil
}

func (b *Bridge) authMethodAllowed(methodID string) bool {
	if len(b.spec.AuthMethods) == 0 {
		return false
	}
	for _, method := range b.spec.AuthMethods {
		if method.ID == methodID {
			return true
		}
	}
	return false
}

func (b *Bridge) logout(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req map[string]any
	if len(params) != 0 && string(params) != "null" {
		if rpcErr := decodeParams(params, &req); rpcErr != nil {
			return nil, rpcErr
		}
	}
	command, err := b.spec.BuildLogout()
	if err != nil {
		return nil, invalidParams("build logout command", err.Error())
	}
	result, err := b.spec.Runner.Run(methodContext(ctx), command)
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "logout command failed", Data: commandErrorData(result, err)}
	}
	return map[string]any{}, nil
}

func (b *Bridge) newSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.NewSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	id, err := b.newID()
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "session id generation failed", Data: err.Error()}
	}
	now := b.now()
	state := &sessionState{Session: Session{
		ID:                    id,
		CWD:                   strings.TrimSpace(req.CWD),
		AdditionalDirectories: cloneStrings(req.AdditionalDirectories),
		MCPServers:            cloneMCPServers(req.MCPServers),
		Config:                defaultConfig(b.spec.Options),
		CreatedAt:             now,
		UpdatedAt:             now,
	}}
	b.mu.Lock()
	b.sessions[id] = state
	b.mu.Unlock()
	if err := b.notifyAvailableCommands(ctx, id); err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "available command notification failed", Data: err.Error()}
	}
	return map[string]any{
		"sessionId":     id,
		"configOptions": b.configOptions(state),
	}, nil
}

func (b *Bridge) forkSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ForkSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	source, rpcErr := b.session(req.SessionID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	id, err := b.newID()
	if err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "session id generation failed", Data: err.Error()}
	}
	now := b.now()
	state := &sessionState{Session: Session{
		ID:                    id,
		CWD:                   strings.TrimSpace(firstNonEmpty(req.CWD, source.CWD)),
		AdditionalDirectories: cloneStrings(firstNonNil(req.AdditionalDirectories, source.AdditionalDirectories)),
		MCPServers:            cloneMCPServers(firstNonNil(req.MCPServers, source.MCPServers)),
		Config:                cloneStringMap(source.Config),
		ModeID:                source.ModeID,
		Title:                 source.Title,
		CreatedAt:             now,
		UpdatedAt:             now,
	}}
	state.transcript = cloneTranscript(source.transcript)
	b.mu.Lock()
	b.sessions[id] = state
	b.mu.Unlock()
	if err := b.notifyAvailableCommands(ctx, id); err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "available command notification failed", Data: err.Error()}
	}
	return map[string]any{
		"sessionId":     id,
		"configOptions": b.configOptions(state),
	}, nil
}

func (b *Bridge) loadSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.LoadSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	state, rpcErr := b.rebindSession(req.SessionID, req.CWD, req.AdditionalDirectories, req.MCPServers)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if err := b.notifyAvailableCommands(ctx, req.SessionID); err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "available command notification failed", Data: err.Error()}
	}
	return map[string]any{"configOptions": b.configOptions(state)}, nil
}

func (b *Bridge) resumeSession(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ResumeSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	state, rpcErr := b.rebindSession(req.SessionID, req.CWD, req.AdditionalDirectories, req.MCPServers)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if err := b.notifyAvailableCommands(ctx, req.SessionID); err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "available command notification failed", Data: err.Error()}
	}
	return map[string]any{"configOptions": b.configOptions(state)}, nil
}

func (b *Bridge) listSessions(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.ListSessionsParams
	if len(params) != 0 && string(params) != "null" {
		if rpcErr := decodeParams(params, &req); rpcErr != nil {
			return nil, rpcErr
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sessions := make([]map[string]any, 0, len(b.sessions))
	for _, state := range b.sessions {
		if req.CWD != "" && state.CWD != req.CWD {
			continue
		}
		item := map[string]any{
			"sessionId": state.ID,
			"cwd":       state.CWD,
		}
		if len(state.AdditionalDirectories) != 0 {
			item["additionalDirectories"] = cloneStrings(state.AdditionalDirectories)
		}
		if title := strings.TrimSpace(state.Title); title != "" {
			item["title"] = title
		}
		if !state.UpdatedAt.IsZero() {
			item["updatedAt"] = state.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		sessions = append(sessions, item)
	}
	sort.Slice(sessions, func(i, j int) bool {
		left, _ := sessions[i]["updatedAt"].(string)
		right, _ := sessions[j]["updatedAt"].(string)
		if left != right {
			return left > right
		}
		return fmt.Sprint(sessions[i]["sessionId"]) < fmt.Sprint(sessions[j]["sessionId"])
	})
	return map[string]any{"sessions": sessions}, nil
}

func (b *Bridge) setConfigOption(ctx *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	req, rpcErr := decodeSetConfigOption(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	b.mu.Lock()
	state := b.sessions[req.SessionID]
	if state == nil {
		b.mu.Unlock()
		return nil, notFound("session not found", req.SessionID)
	}
	option, ok := b.selectOption(req.ConfigID)
	if !ok {
		b.mu.Unlock()
		return nil, invalidParams("unknown config option", req.ConfigID)
	}
	if !selectOptionAllows(option, req.Value) {
		b.mu.Unlock()
		return nil, invalidParams("unsupported config value", req.Value)
	}
	state.Config[req.ConfigID] = req.Value
	state.UpdatedAt = b.now()
	configOptions := b.configOptions(state)
	b.mu.Unlock()
	if err := notifyConfigOptions(ctx, req.SessionID, configOptions); err != nil {
		return nil, &acp.RPCError{Code: -32000, Message: "config option notification failed", Data: err.Error()}
	}
	return map[string]any{"configOptions": configOptions}, nil
}

func (b *Bridge) setMode(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.SetModeParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[req.SessionID]
	if state == nil {
		return nil, notFound("session not found", req.SessionID)
	}
	state.ModeID = req.ModeID
	state.UpdatedAt = b.now()
	return map[string]any{}, nil
}

func (b *Bridge) prompt(ctx *acp.MethodContext, params json.RawMessage) (response any, rpcErr *acp.RPCError) {
	var req runtimeacp.PromptParams
	if decodeErr := decodeParams(params, &req); decodeErr != nil {
		return nil, decodeErr
	}
	if b.spec.BuildPrompt == nil {
		return nil, &acp.RPCError{Code: -32004, Message: "not implemented", Data: "prompt command builder is not configured"}
	}
	state, sessionErr := b.session(req.SessionID)
	if sessionErr != nil {
		return nil, sessionErr
	}
	baseCtx := methodContext(ctx)
	if b.promptMethodContext != nil {
		baseCtx = b.promptMethodContext(ctx)
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if err := baseCtx.Err(); err != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	runCtx, cancel := context.WithCancel(baseCtx)
	promptToken, beginErr := b.beginPrompt(req.SessionID, cancel)
	if beginErr != nil {
		cancel()
		return nil, beginErr
	}
	defer b.endPrompt(req.SessionID, promptToken)
	defer cancel()
	if runCtx.Err() != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}

	preparedParams, stage, err := preparePromptResources(runCtx, req, b.spec.PromptResourceLimits, b.spec.PromptResourceTempDir, b.promptResourceCleanupHook)
	if err != nil {
		var cleanupErr *promptResourceCleanupError
		if errors.As(err, &cleanupErr) {
			return nil, &acp.RPCError{Code: -32000, Message: "prompt resource cleanup failed", Data: cleanupErr.Error()}
		}
		var stagingErr *promptResourceStagingError
		if errors.As(err, &stagingErr) {
			return nil, &acp.RPCError{Code: -32000, Message: "prompt resource staging failed", Data: stagingErr.Error()}
		}
		if runCtx.Err() != nil {
			return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
		}
		return nil, invalidParams("prepare prompt resources", err.Error())
	}
	if stage != nil {
		defer func() {
			if err := stage.cleanup(); err != nil {
				response = nil
				rpcErr = &acp.RPCError{Code: -32000, Message: "prompt resource cleanup failed", Data: err.Error()}
			}
		}()
	}
	stageDir := ""
	if stage != nil {
		stageDir = stage.dir
	}
	promptRedactor := newPromptResourceRedactor(preparedParams, stageDir)
	promptParams := b.promptParamsForSession(state, preparedParams)
	promptSession := state.Session
	if stage != nil {
		promptSession.AdditionalDirectories = append(cloneStrings(promptSession.AdditionalDirectories), stage.dir)
		if b.promptStagePrepared != nil {
			b.promptStagePrepared(stage)
		}
		if verifyErr := stage.verify(); verifyErr != nil {
			return nil, &acp.RPCError{Code: -32000, Message: "prompt resource stage verification failed", Data: promptRedactor.RedactFinal(verifyErr.Error())}
		}
	}
	if runCtx.Err() != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	command, err := b.spec.BuildPrompt(promptSession, promptParams)
	if err != nil {
		return nil, invalidParams("build prompt command", promptRedactor.RedactFinal(err.Error()))
	}
	parser := b.newStreamParser(state.Session, promptParams)
	if runCtx.Err() != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	if stage != nil {
		if verifyErr := stage.verify(); verifyErr != nil {
			return nil, &acp.RPCError{Code: -32000, Message: "prompt resource stage verification failed", Data: promptRedactor.RedactFinal(verifyErr.Error())}
		}
	}
	result, assistantText, err := b.runPromptCommand(runCtx, ctx, req.SessionID, command, parser, promptRedactor)
	if runCtx.Err() != nil {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	if err != nil {
		if b.authRequired(result, err) {
			return nil, authRequired(commandErrorData(
				redactPromptCommandResult(result, promptRedactor.RedactFinal),
				redactPromptCommandError(err, promptRedactor.RedactFinal),
			))
		}
		data := commandErrorData(
			redactPromptCommandResult(result, promptRedactor.RedactFinal),
			redactPromptCommandError(err, promptRedactor.RedactFinal),
		)
		if kind := b.classifyPromptFailure(state.Session, command, result, err); kind != "" {
			data["errorKind"] = kind
		}
		return nil, &acp.RPCError{Code: -32000, Message: "prompt command failed", Data: data}
	}
	assistantTranscriptText := promptRedactor.RedactFinal(assistantText)
	if stage != nil {
		if cleanupErr := stage.cleanup(); cleanupErr != nil {
			return nil, &acp.RPCError{Code: -32000, Message: "prompt resource cleanup failed", Data: cleanupErr.Error()}
		}
	}
	info, notifyInfo, committed := b.finalizePromptSuccess(req.SessionID, promptToken, runCtx, promptTranscriptText(req), assistantTranscriptText)
	if !committed {
		return runtimeacp.PromptResult{StopReason: runtimeacp.StopReasonCancelled}, nil
	}
	if notifyInfo {
		if err := notifySessionInfo(ctx, req.SessionID, info); err != nil {
			return nil, &acp.RPCError{Code: -32000, Message: "session info notification failed", Data: err.Error()}
		}
	}
	stopReason := runtimeacp.StopReasonEndTurn
	if parser != nil {
		if parsed := parser.StopReason(); parsed != "" {
			stopReason = parsed
		}
	}
	return runtimeacp.PromptResult{StopReason: stopReason}, nil
}

func (b *Bridge) runPromptCommand(runCtx context.Context, methodCtx *acp.MethodContext, sessionID string, command adapterprocess.Spec, parser StreamParser, redactor promptResourceRedactor) (adapterprocess.Result, string, error) {
	if err := runCtx.Err(); err != nil {
		return adapterprocess.Result{}, "", err
	}
	redactFinal := redactor.RedactFinal
	notificationCommand := command
	notificationCommand.Command = redactFinal(notificationCommand.Command)
	notificationCommand.Dir = redactFinal(notificationCommand.Dir)
	toolCallID := b.newToolCallID()
	if err := notifyPromptToolCallStart(methodCtx, sessionID, toolCallID, notificationCommand); err != nil {
		return adapterprocess.Result{}, "", fmt.Errorf("notify prompt tool start: %w", err)
	}
	if contextErr := runCtx.Err(); contextErr != nil {
		result := promptCommandResultForFinish(adapterprocess.Result{}, contextErr)
		if notifyErr := notifyPromptToolCallFinish(methodCtx, sessionID, toolCallID, notificationCommand, result, contextErr, contextErr); notifyErr != nil {
			return adapterprocess.Result{}, "", fmt.Errorf("notify cancelled prompt tool finish: %w", notifyErr)
		}
		return adapterprocess.Result{}, "", contextErr
	}

	if runner, ok := b.spec.Runner.(StreamRunner); ok {
		var assistantText strings.Builder
		streamRedactor := redactor.Stream()
		result, err := runner.RunStream(runCtx, command, func(chunk []byte) error {
			if contextErr := runCtx.Err(); contextErr != nil {
				return contextErr
			}
			if parser == nil {
				assistantText.Write(chunk)
				if contextErr := runCtx.Err(); contextErr != nil {
					return contextErr
				}
				text := streamRedactor.Push(string(chunk))
				if text == "" {
					return nil
				}
				if contextErr := runCtx.Err(); contextErr != nil {
					return contextErr
				}
				return notifyAgentMessageChunk(methodCtx, sessionID, text)
			}
			events, parseErr := parser.Parse(chunk)
			if parseErr != nil {
				return parseErr
			}
			return handleStreamEvents(runCtx, methodCtx, sessionID, redactPromptStreamEvents(events, redactFinal))
		})
		if parser == nil {
			if runCtx.Err() == nil && err == nil {
				if text := streamRedactor.Flush(); text != "" {
					if contextErr := runCtx.Err(); contextErr != nil {
						err = contextErr
					} else if notifyErr := notifyAgentMessageChunk(methodCtx, sessionID, text); notifyErr != nil {
						err = fmt.Errorf("notification failed: %w", notifyErr)
					}
				}
			} else {
				streamRedactor.Discard()
			}
		}
		if parser != nil && runCtx.Err() == nil {
			events, flushErr := parser.Flush()
			if runCtx.Err() == nil {
				if flushErr != nil && err == nil {
					err = flushErr
				}
				if notifyErr := handleStreamEvents(runCtx, methodCtx, sessionID, redactPromptStreamEvents(events, redactFinal)); notifyErr != nil && err == nil {
					err = notifyErr
				}
				if runCtx.Err() == nil {
					assistantText.WriteString(parser.Transcript())
				}
			}
		}
		finishResult := result
		if parser != nil {
			finishResult.Stdout = nil
			finishResult.StdoutTruncated = false
		}
		finishResult = promptCommandResultForFinish(finishResult, runCtx.Err())
		finishResult = redactPromptCommandResult(finishResult, redactFinal)
		if notifyErr := notifyPromptToolCallFinish(methodCtx, sessionID, toolCallID, notificationCommand, finishResult, redactPromptCommandError(err, redactFinal), runCtx.Err()); notifyErr != nil && err == nil {
			err = fmt.Errorf("notify prompt tool finish: %w", notifyErr)
		}
		return result, assistantText.String(), err
	}
	result, err := b.spec.Runner.Run(runCtx, command)
	contextErr := runCtx.Err()
	text := strings.TrimSpace(redactFinal(string(result.Stdout)))
	if contextErr == nil && text != "" {
		if contextErr = runCtx.Err(); contextErr != nil {
			text = ""
		} else if notifyErr := notifyAgentMessageChunk(methodCtx, sessionID, text); notifyErr != nil && err == nil {
			err = fmt.Errorf("notification failed: %w", notifyErr)
		}
	}
	contextErr = runCtx.Err()
	finishResult := promptCommandResultForFinish(result, contextErr)
	finishResult = redactPromptCommandResult(finishResult, redactFinal)
	if notifyErr := notifyPromptToolCallFinish(methodCtx, sessionID, toolCallID, notificationCommand, finishResult, redactPromptCommandError(err, redactFinal), contextErr); notifyErr != nil && err == nil {
		err = fmt.Errorf("notify prompt tool finish: %w", notifyErr)
	}
	return result, string(result.Stdout), err
}

func promptCommandResultForFinish(result adapterprocess.Result, contextErr error) adapterprocess.Result {
	if contextErr == nil {
		return result
	}
	result.Stdout = nil
	result.Stderr = nil
	result.StdoutTruncated = false
	result.StderrTruncated = false
	return result
}

func redactPromptCommandResult(result adapterprocess.Result, redact func(string) string) adapterprocess.Result {
	if redact == nil {
		return result
	}
	if result.Stdout != nil {
		result.Stdout = []byte(redact(string(result.Stdout)))
	}
	if result.Stderr != nil {
		result.Stderr = []byte(redact(string(result.Stderr)))
	}
	return result
}

func redactPromptCommandError(err error, redact func(string) string) error {
	if err == nil || redact == nil {
		return err
	}
	return errors.New(redact(err.Error()))
}

func redactPromptStreamEvents(events []StreamEvent, redact func(string) string) []StreamEvent {
	if len(events) == 0 || redact == nil {
		return events
	}
	out := make([]StreamEvent, len(events))
	for index, event := range events {
		out[index] = event
		if event.Update != nil {
			out[index].Update = redactPromptStringValues(event.Update, redact).(map[string]any)
		}
		if event.PermissionRequest != nil {
			request := *event.PermissionRequest
			request.ToolCallID = redact(request.ToolCallID)
			request.Title = redact(request.Title)
			request.Kind = redact(request.Kind)
			request.RawInput = redactPromptStringValues(request.RawInput, redact)
			request.Options = append([]PermissionOption(nil), request.Options...)
			for optionIndex := range request.Options {
				request.Options[optionIndex].OptionID = redact(request.Options[optionIndex].OptionID)
				request.Options[optionIndex].Name = redact(request.Options[optionIndex].Name)
				request.Options[optionIndex].Kind = redact(request.Options[optionIndex].Kind)
			}
			out[index].PermissionRequest = &request
		}
	}
	return out
}

func redactPromptStringValues(value any, redact func(string) string) any {
	switch typed := value.(type) {
	case string:
		return redact(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[redact(key)] = redactPromptStringValues(item, redact)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			out[redact(key)] = redact(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = redactPromptStringValues(item, redact)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for index, item := range typed {
			out[index] = redact(item)
		}
		return out
	case json.RawMessage:
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(typed))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return json.RawMessage(redact(string(typed)))
		}
		redacted, err := json.Marshal(redactPromptStringValues(decoded, redact))
		if err != nil {
			return json.RawMessage(redact(string(typed)))
		}
		return json.RawMessage(redacted)
	case []byte:
		return []byte(redact(string(typed)))
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return value
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return value
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return value
		}
		return redactPromptStringValues(decoded, redact)
	}
}

func (b *Bridge) newStreamParser(session Session, params runtimeacp.PromptParams) StreamParser {
	if b.spec.NewStreamParser == nil {
		return nil
	}
	return b.spec.NewStreamParser(session, params)
}

func (b *Bridge) promptParamsForSession(state *sessionState, req runtimeacp.PromptParams) runtimeacp.PromptParams {
	if !b.spec.IncludeTranscript || len(state.transcript) == 0 {
		return req
	}
	history := transcriptHistoryPrelude(state.transcript)
	if history == "" {
		return req
	}
	out := req
	out.Prompt = make([]runtimeacp.ContentBlock, 0, len(req.Prompt)+1)
	out.Prompt = append(out.Prompt, runtimeacp.ContentBlock{Type: "text", Text: history})
	out.Prompt = append(out.Prompt, req.Prompt...)
	return out
}

func (b *Bridge) finalizePromptSuccess(sessionID string, prompt *activePrompt, runCtx context.Context, userText, assistantText string) (sessionInfo, bool, bool) {
	userText = strings.TrimSpace(userText)
	assistantText = strings.TrimSpace(assistantText)
	b.mu.Lock()
	defer b.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil || prompt == nil || prompt.finalized || b.active[sessionID] != prompt {
		return sessionInfo{}, false, false
	}
	state := b.sessions[sessionID]
	if state == nil {
		return sessionInfo{}, false, false
	}
	prompt.finalized = true
	state.PromptCount++
	if !b.spec.IncludeTranscript || userText == "" && assistantText == "" {
		return sessionInfo{}, false, true
	}
	if state.Title == "" {
		state.Title = sessionTitle(userText)
	}
	state.UpdatedAt = b.now()
	state.transcript = append(state.transcript, transcriptExchange{
		User:      userText,
		Assistant: assistantText,
	})
	if max := b.maxTranscriptExchanges(); max > 0 && len(state.transcript) > max {
		state.transcript = append([]transcriptExchange(nil), state.transcript[len(state.transcript)-max:]...)
	}
	return sessionInfo{Title: state.Title, UpdatedAt: state.UpdatedAt}, true, true
}

func sessionTitle(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if len(title) > 80 {
		return title[:80] + "..."
	}
	return title
}

func (b *Bridge) maxTranscriptExchanges() int {
	if b.spec.MaxTranscriptExchanges > 0 {
		return b.spec.MaxTranscriptExchanges
	}
	return defaultMaxTranscriptExchanges
}

func transcriptHistoryPrelude(history []transcriptExchange) string {
	var builder strings.Builder
	builder.WriteString("Previous conversation:\n")
	for _, exchange := range history {
		if user := strings.TrimSpace(exchange.User); user != "" {
			builder.WriteString("\nUser:\n")
			builder.WriteString(user)
			builder.WriteString("\n")
		}
		if assistant := strings.TrimSpace(exchange.Assistant); assistant != "" {
			builder.WriteString("\nAssistant:\n")
			builder.WriteString(assistant)
			builder.WriteString("\n")
		}
	}
	builder.WriteString("\nCurrent user request:")
	return strings.TrimSpace(builder.String())
}

func notifyPromptToolCallStart(ctx *acp.MethodContext, sessionID, toolCallID string, command adapterprocess.Spec) error {
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    toolCallID,
			"title":         promptCommandTitle(command),
			"kind":          "execute",
			"status":        "in_progress",
			"rawInput":      promptCommandRawInput(command),
		},
	})
}

func notifyPromptToolCallFinish(ctx *acp.MethodContext, sessionID, toolCallID string, command adapterprocess.Spec, result adapterprocess.Result, runErr, contextErr error) error {
	status := "completed"
	if runErr != nil || contextErr != nil {
		status = "failed"
	}
	update := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    toolCallID,
		"title":         promptCommandTitle(command),
		"kind":          "execute",
		"status":        status,
		"rawInput":      promptCommandRawInput(command),
	}
	if content := promptCommandToolContent(result, runErr, contextErr); len(content) > 0 {
		update["content"] = content
	}
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update":    update,
	})
}

func notifyAgentMessageChunk(ctx *acp.MethodContext, sessionID string, text string) error {
	if text == "" {
		return nil
	}
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{
				"type": "text",
				"text": text,
			},
		},
	})
}

func handleStreamEvents(runCtx context.Context, ctx *acp.MethodContext, sessionID string, events []StreamEvent) error {
	for _, event := range events {
		if err := runCtx.Err(); err != nil {
			return err
		}
		if len(event.Update) > 0 {
			if err := ctx.Notify("session/update", map[string]any{
				"sessionId": sessionID,
				"update":    event.Update,
			}); err != nil {
				return err
			}
		}
		if event.PermissionRequest != nil {
			if err := runCtx.Err(); err != nil {
				return err
			}
			if err := requestStreamPermission(runCtx, ctx, sessionID, *event.PermissionRequest); err != nil {
				return err
			}
		}
	}
	return nil
}

func requestStreamPermission(runCtx context.Context, ctx *acp.MethodContext, sessionID string, req PermissionRequest) error {
	raw, rpcErr, err := ctx.RequestContext(runCtx, "session/request_permission", permissionRequestParams(sessionID, req))
	if err != nil {
		return fmt.Errorf("request permission: %w", err)
	}
	if rpcErr != nil {
		return fmt.Errorf("request permission: %s", rpcErr.Message)
	}
	outcome, err := decodePermissionOutcome(raw)
	if err != nil {
		return err
	}
	switch outcome.Outcome {
	case "selected":
		if strings.TrimSpace(outcome.OptionID) == "" {
			return fmt.Errorf("permission response missing selected option for %s", permissionRequestTitle(req))
		}
		option, ok := findPermissionOption(req.Options, outcome.OptionID)
		if !ok {
			return fmt.Errorf("permission response selected unknown option %q for %s", outcome.OptionID, permissionRequestTitle(req))
		}
		if permissionOptionAllows(option) {
			return nil
		}
		return fmt.Errorf("permission rejected for %s", permissionRequestTitle(req))
	case "cancelled":
		return fmt.Errorf("permission cancelled for %s", permissionRequestTitle(req))
	default:
		return fmt.Errorf("permission response outcome %q is not supported", outcome.Outcome)
	}
}

func permissionRequestParams(sessionID string, req PermissionRequest) map[string]any {
	options := normalizePermissionOptions(req.Options)
	toolCall := map[string]any{
		"toolCallId": permissionToolCallID(req),
		"title":      permissionRequestTitle(req),
		"kind":       firstNonEmpty(req.Kind, "execute"),
		"status":     "pending",
	}
	if req.RawInput != nil {
		toolCall["rawInput"] = req.RawInput
	}
	return map[string]any{
		"sessionId": sessionID,
		"toolCall":  toolCall,
		"options":   permissionOptionParams(options),
	}
}

func permissionOptionParams(options []PermissionOption) []map[string]string {
	out := make([]map[string]string, 0, len(options))
	for _, option := range options {
		out = append(out, map[string]string{
			"optionId": option.OptionID,
			"name":     option.Name,
			"kind":     option.Kind,
		})
	}
	return out
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

func decodePermissionOutcome(raw json.RawMessage) (permissionOutcome, error) {
	var response struct {
		Outcome permissionOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return permissionOutcome{}, fmt.Errorf("decode permission response: %w", err)
	}
	if response.Outcome.Outcome == "" {
		return permissionOutcome{}, errors.New("permission response missing outcome")
	}
	return response.Outcome, nil
}

func findPermissionOption(options []PermissionOption, optionID string) (PermissionOption, bool) {
	for _, option := range normalizePermissionOptions(options) {
		if option.OptionID == optionID {
			return option, true
		}
	}
	return PermissionOption{}, false
}

func permissionOptionAllows(option PermissionOption) bool {
	kind := strings.ToLower(strings.TrimSpace(option.Kind))
	id := strings.ToLower(strings.TrimSpace(option.OptionID))
	return strings.HasPrefix(kind, "allow") || strings.HasPrefix(id, "allow")
}

func permissionRequestTitle(req PermissionRequest) string {
	return firstNonEmpty(req.Title, permissionToolCallID(req))
}

func permissionToolCallID(req PermissionRequest) string {
	if strings.TrimSpace(req.ToolCallID) != "" {
		return strings.TrimSpace(req.ToolCallID)
	}
	if strings.TrimSpace(req.Title) != "" {
		return strings.TrimSpace(req.Title)
	}
	return "permission-request"
}

func notifyConfigOptions(ctx *acp.MethodContext, sessionID string, configOptions []map[string]any) error {
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "config_option_update",
			"configOptions": configOptions,
		},
	})
}

func notifySessionInfo(ctx *acp.MethodContext, sessionID string, info sessionInfo) error {
	update := map[string]any{
		"sessionUpdate": "session_info_update",
	}
	if title := strings.TrimSpace(info.Title); title != "" {
		update["title"] = title
	}
	if !info.UpdatedAt.IsZero() {
		update["updatedAt"] = info.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update":    update,
	})
}

func (b *Bridge) newToolCallID() string {
	return fmt.Sprintf("prompt-command-%d", b.nextID.Add(1))
}

func promptCommandTitle(command adapterprocess.Spec) string {
	name := promptCommandExecutable(command)
	if name == "" {
		return "Run prompt command"
	}
	return "Run " + name
}

func promptCommandExecutable(command adapterprocess.Spec) string {
	name := strings.TrimSpace(command.Command)
	if base := filepath.Base(name); base != "." && base != string(filepath.Separator) && base != "" {
		name = base
	}
	return name
}

func promptCommandRawInput(command adapterprocess.Spec) map[string]any {
	input := map[string]any{
		"command": promptCommandExecutable(command),
	}
	if len(command.Args) > 0 {
		input["arguments"] = adapterprocess.RedactedValue
	}
	if dir := strings.TrimSpace(command.Dir); dir != "" {
		input["cwd"] = dir
	}
	return input
}

func promptCommandToolContent(result adapterprocess.Result, runErr, contextErr error) []map[string]any {
	parts := make([]string, 0, 3)
	if text, truncated := limitedToolOutput("stdout", result.Stdout, result.StdoutTruncated); text != "" {
		parts = append(parts, text)
		if truncated {
			parts = append(parts, "stdout truncated")
		}
	}
	if text, truncated := limitedToolOutput("stderr", result.Stderr, result.StderrTruncated); text != "" {
		parts = append(parts, text)
		if truncated {
			parts = append(parts, "stderr truncated")
		}
	}
	if contextErr != nil {
		parts = append(parts, "cancelled: "+contextErr.Error())
	} else if runErr != nil && len(parts) == 0 {
		parts = append(parts, "error: "+runErr.Error())
	}
	if len(parts) == 0 {
		return nil
	}
	return []map[string]any{{
		"type": "content",
		"content": map[string]any{
			"type": "text",
			"text": strings.Join(parts, "\n\n"),
		},
	}}
}

func limitedToolOutput(label string, raw []byte, alreadyTruncated bool) (string, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", alreadyTruncated
	}
	truncated := alreadyTruncated
	if len(text) > toolOutputPreviewLimit {
		text = text[:toolOutputPreviewLimit]
		truncated = true
	}
	return label + ":\n" + text, truncated
}

func (b *Bridge) cancelMethod(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CancelParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"cancelled": b.cancel(req.SessionID)}, nil
}

func (b *Bridge) cancelNotification(params json.RawMessage) error {
	var req runtimeacp.CancelParams
	if err := json.Unmarshal(params, &req); err != nil {
		return fmt.Errorf("invalid session/cancel params: %w", err)
	}
	b.cancel(req.SessionID)
	return nil
}

func (b *Bridge) closeSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.CloseSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.cancel(req.SessionID)
	b.mu.Lock()
	delete(b.sessions, req.SessionID)
	b.mu.Unlock()
	return map[string]any{}, nil
}

func (b *Bridge) deleteSession(_ *acp.MethodContext, params json.RawMessage) (any, *acp.RPCError) {
	var req runtimeacp.DeleteSessionParams
	if rpcErr := decodeParams(params, &req); rpcErr != nil {
		return nil, rpcErr
	}
	b.cancel(req.SessionID)
	b.mu.Lock()
	delete(b.sessions, req.SessionID)
	b.mu.Unlock()
	return map[string]any{}, nil
}

func (b *Bridge) session(id string) (*sessionState, *acp.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[id]
	if state == nil {
		return nil, notFound("session not found", id)
	}
	clone := *state
	clone.Config = cloneStringMap(state.Config)
	clone.AdditionalDirectories = cloneStrings(state.AdditionalDirectories)
	clone.MCPServers = cloneMCPServers(state.MCPServers)
	clone.transcript = cloneTranscript(state.transcript)
	return &clone, nil
}

func (b *Bridge) rebindSession(id, cwd string, additionalDirectories []string, mcpServers []runtimeacp.MCPServer) (*sessionState, *acp.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.sessions[id]
	if state == nil {
		if !b.spec.LoadUnknownSessions {
			return nil, notFound("session not found", id)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, &acp.RPCError{Code: -32602, Message: "session id is required"}
		}
		now := b.now()
		state = &sessionState{Session: Session{
			ID:                    id,
			CWD:                   strings.TrimSpace(cwd),
			AdditionalDirectories: cloneStrings(additionalDirectories),
			MCPServers:            cloneMCPServers(mcpServers),
			Config:                defaultConfig(b.spec.Options),
			CreatedAt:             now,
			UpdatedAt:             now,
			Adopted:               true,
		}}
		b.sessions[id] = state
		cwd = ""
		additionalDirectories = nil
		mcpServers = nil
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		state.CWD = cwd
	}
	if additionalDirectories != nil {
		state.AdditionalDirectories = cloneStrings(additionalDirectories)
	}
	if mcpServers != nil {
		state.MCPServers = cloneMCPServers(mcpServers)
	}
	clone := *state
	clone.Config = cloneStringMap(state.Config)
	clone.AdditionalDirectories = cloneStrings(state.AdditionalDirectories)
	clone.MCPServers = cloneMCPServers(state.MCPServers)
	clone.transcript = cloneTranscript(state.transcript)
	return &clone, nil
}

func (b *Bridge) notifyAvailableCommands(ctx *acp.MethodContext, sessionID string) error {
	if len(b.spec.Commands) == 0 {
		return nil
	}
	commands := make([]map[string]any, 0, len(b.spec.Commands))
	for _, command := range b.spec.Commands {
		name := strings.TrimSpace(command.Name)
		if name == "" {
			continue
		}
		item := map[string]any{
			"name": name,
		}
		if description := strings.TrimSpace(command.Description); description != "" {
			item["description"] = description
		}
		if hint := strings.TrimSpace(command.InputHint); hint != "" {
			item["input"] = map[string]any{
				"unstructured": map[string]any{"hint": hint},
			}
		}
		commands = append(commands, item)
	}
	if len(commands) == 0 {
		return nil
	}
	return ctx.Notify("session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate":     "available_commands_update",
			"availableCommands": commands,
		},
	})
}

func (b *Bridge) beginPrompt(sessionID string, cancel context.CancelFunc) (*activePrompt, *acp.RPCError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active[sessionID] != nil {
		return nil, &acp.RPCError{Code: -32009, Message: "session busy", Data: sessionID}
	}
	prompt := &activePrompt{cancel: cancel}
	b.active[sessionID] = prompt
	return prompt, nil
}

func (b *Bridge) endPrompt(sessionID string, prompt *activePrompt) {
	b.mu.Lock()
	if b.active[sessionID] == prompt {
		delete(b.active, sessionID)
	}
	b.mu.Unlock()
}

func (b *Bridge) cancel(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	prompt := b.active[sessionID]
	if prompt == nil || prompt.finalized || prompt.cancel == nil {
		return false
	}
	prompt.cancel()
	return true
}

func (b *Bridge) newID() (string, error) {
	if b.spec.NewID != nil {
		id := b.spec.NewID()
		if strings.TrimSpace(id) == "" {
			return "", errors.New("custom session id is empty")
		}
		return id, nil
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("read secure session id entropy: %w", err)
	}
	return "session-" + hex.EncodeToString(entropy[:]), nil
}

func (b *Bridge) now() time.Time {
	if b.spec.Now != nil {
		return b.spec.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Bridge) selectOption(id string) (SelectConfigOption, bool) {
	for _, option := range b.spec.Options {
		if option.ID == id {
			return option, true
		}
	}
	return SelectConfigOption{}, false
}

func (b *Bridge) configOptions(state *sessionState) []map[string]any {
	if len(b.spec.Options) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(b.spec.Options))
	for _, option := range b.spec.Options {
		current := state.Config[option.ID]
		if current == "" {
			current = option.DefaultValue
		}
		values := make([]map[string]any, 0, len(option.Options))
		for _, value := range option.Options {
			item := map[string]any{
				"value": value.Value,
				"name":  value.Name,
			}
			if value.Description != "" {
				item["description"] = value.Description
			}
			values = append(values, item)
		}
		item := map[string]any{
			"type":         "select",
			"id":           option.ID,
			"name":         option.Name,
			"currentValue": current,
			"options":      values,
		}
		if option.Description != "" {
			item["description"] = option.Description
		}
		if option.Category != "" {
			item["category"] = option.Category
		}
		out = append(out, item)
	}
	return out
}

func defaultConfig(options []SelectConfigOption) map[string]string {
	out := make(map[string]string, len(options))
	for _, option := range options {
		if option.ID != "" && option.DefaultValue != "" {
			out[option.ID] = option.DefaultValue
		}
	}
	return out
}

func selectOptionAllows(option SelectConfigOption, value string) bool {
	for _, candidate := range option.Options {
		if candidate.Value == value {
			return true
		}
	}
	return false
}

type setConfigRequest struct {
	SessionID string
	ConfigID  string
	Value     string
}

func decodeSetConfigOption(params json.RawMessage) (setConfigRequest, *acp.RPCError) {
	var req struct {
		SessionID string          `json:"sessionId"`
		ConfigID  string          `json:"configId"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return setConfigRequest{}, invalidParams("invalid params", err.Error())
	}
	if req.SessionID == "" {
		return setConfigRequest{}, invalidParams("session id is required", nil)
	}
	if req.ConfigID == "" {
		return setConfigRequest{}, invalidParams("config id is required", nil)
	}
	var value string
	if err := json.Unmarshal(req.Value, &value); err != nil {
		return setConfigRequest{}, invalidParams("value must be a string", err.Error())
	}
	if value == "" {
		return setConfigRequest{}, invalidParams("value is required", nil)
	}
	return setConfigRequest{SessionID: req.SessionID, ConfigID: req.ConfigID, Value: value}, nil
}

func decodeParams(params json.RawMessage, target any) *acp.RPCError {
	if err := json.Unmarshal(params, target); err != nil {
		return invalidParams("invalid params", err.Error())
	}
	return nil
}

func methodContext(ctx *acp.MethodContext) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx.Context()
}

func notFound(message string, data any) *acp.RPCError {
	return &acp.RPCError{Code: -32001, Message: message, Data: data}
}

func invalidParams(message string, data any) *acp.RPCError {
	return &acp.RPCError{Code: -32602, Message: message, Data: data}
}

func authRequired(data any) *acp.RPCError {
	return &acp.RPCError{Code: -32000, Message: "Authentication required", Data: data}
}

func (b *Bridge) authRequired(result adapterprocess.Result, err error) bool {
	return b != nil && b.spec.AuthRequired != nil && b.spec.AuthRequired(result, err)
}

func (b *Bridge) classifyPromptFailure(session Session, command adapterprocess.Spec, result adapterprocess.Result, err error) string {
	if b == nil || b.spec.ClassifyPromptFailure == nil {
		return ""
	}
	var exitErr *adapterprocess.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code == 0 {
		return ""
	}
	switch b.spec.ClassifyPromptFailure(session, command, result, err) {
	case PromptFailureNativeSessionMissing:
		return "native_session_missing"
	default:
		return ""
	}
}

func commandErrorData(result adapterprocess.Result, err error) map[string]any {
	data := map[string]any{
		"error": err.Error(),
	}
	stderr := strings.TrimSpace(string(result.Stderr))
	if stderr != "" {
		data["stderr"] = stderr
	}
	if result.StderrTruncated {
		data["stderr_truncated"] = true
	}
	return data
}

// PromptText renders a prepared prompt for legacy callers. It returns an empty
// string when any block is unsupported or has not passed private preparation.
// New prompt builders should call RequirePromptText and handle its error, or
// use PreparedPromptInputs for provider-specific argument construction.
func PromptText(params runtimeacp.PromptParams) string {
	text, err := renderPreparedPrompt(params)
	if err != nil {
		return ""
	}
	return text
}

func legacyPromptText(params runtimeacp.PromptParams) string {
	return renderPromptText(params, true)
}

func transcriptPromptText(params runtimeacp.PromptParams) string {
	return renderPromptText(params, false)
}

func sanitizeTranscriptAssistantText(params runtimeacp.PromptParams, text string) string {
	aliases := resourceLinkAliases(params)
	for _, alias := range aliases {
		text = replaceAllResourceLinkAlias(text, alias, "[attachment path omitted]")
	}
	return text
}

func replaceAllResourceLinkAlias(value, alias, replacement string) string {
	if value == "" || alias == "" || len(alias) > len(value) {
		return value
	}
	var out strings.Builder
	matched := false
	for offset := 0; offset < len(value); {
		if offset+len(alias) <= len(value) && resourceLinkAliasEqual(value[offset:offset+len(alias)], alias) {
			if !matched {
				out.Grow(len(value))
			}
			out.WriteString(replacement)
			offset += len(alias)
			matched = true
			continue
		}
		out.WriteByte(value[offset])
		offset++
	}
	if !matched {
		return value
	}
	return out.String()
}

func resourceLinkAliasEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	left = strings.ReplaceAll(left, `\`, "/")
	right = strings.ReplaceAll(right, `\`, "/")
	return strings.EqualFold(left, right)
}

func resourceLinkAliases(params runtimeacp.PromptParams) []string {
	seen := map[string]struct{}{}
	addRaw := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "." || value == "/" {
			return
		}
		seen[value] = struct{}{}
	}
	add := func(value string) {
		addRaw(value)
		if encoded, err := json.Marshal(strings.TrimSpace(value)); err == nil && len(encoded) >= 2 {
			addRaw(string(encoded[1 : len(encoded)-1]))
		}
	}
	addPath := func(value string) {
		value = strings.TrimSpace(value)
		add(value)
		add(filepath.FromSlash(value))
		add(strings.ReplaceAll(value, "/", `\`))
		if len(value) >= 3 && value[0] == '/' && isASCIILetter(value[1]) && value[2] == ':' {
			drivePath := value[1:]
			add(drivePath)
			add(strings.ReplaceAll(drivePath, "/", `\`))
		}
	}
	addPathAndParent := func(value string) {
		value = strings.TrimSpace(value)
		addPath(value)
		parent := pathpkg.Dir(strings.ReplaceAll(value, `\`, "/"))
		if parent == "." || parent == "/" {
			return
		}
		addPath(parent)
		add(pathpkg.Base(parent))
		if len(parent) >= 3 && parent[0] == '/' && isASCIILetter(parent[1]) && parent[2] == ':' {
			driveParent := parent[1:]
			add(driveParent)
			add(strings.ReplaceAll(driveParent, "/", `\`))
			add(pathpkg.Base(driveParent))
		}
	}
	for _, block := range params.Prompt {
		if block.Type != "resource_link" {
			continue
		}
		raw := strings.TrimSpace(block.URI)
		add(raw)
		parsed, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(parsed.Scheme, "file") {
			continue
		}
		for _, path := range []string{parsed.Path, parsed.EscapedPath()} {
			addPathAndParent(path)
			if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
				uncPath := "//" + parsed.Host + path
				addPathAndParent(uncPath)
			}
		}
		parentPath := pathpkg.Dir(parsed.Path)
		if parentPath != "." && parentPath != "/" {
			parentURI := *parsed
			parentURI.Path = parentPath
			parentURI.RawPath = ""
			parentURI.RawQuery = ""
			parentURI.ForceQuery = false
			parentURI.Fragment = ""
			parentURI.RawFragment = ""
			add(parentURI.String())
		}
	}
	aliases := make([]string, 0, len(seen))
	for alias := range seen {
		aliases = append(aliases, alias)
	}
	sort.Slice(aliases, func(i, j int) bool {
		if len(aliases[i]) == len(aliases[j]) {
			return aliases[i] < aliases[j]
		}
		return len(aliases[i]) > len(aliases[j])
	})
	return aliases
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func renderPromptText(params runtimeacp.PromptParams, includeResourceLinkURI bool) string {
	var parts []string
	for _, block := range params.Prompt {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		case "resource":
			if block.Resource != nil {
				if text := strings.TrimSpace(block.Resource.Text); text != "" {
					parts = append(parts, text)
				}
			}
		case "resource_link":
			if attachment := resourceLinkPromptText(block, includeResourceLinkURI); attachment != "" {
				parts = append(parts, attachment)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func resourceLinkPromptText(block runtimeacp.ContentBlock, includeURI bool) string {
	uri := strings.TrimSpace(block.URI)
	if includeURI && uri == "" {
		return ""
	}
	name := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(block.Name))
	mediaType := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(block.MimeType))
	if !includeURI {
		switch {
		case name != "" && mediaType != "":
			return fmt.Sprintf("Attached file %q (%s) was provided for this turn.", name, mediaType)
		case name != "":
			return fmt.Sprintf("Attached file %q was provided for this turn.", name)
		case mediaType != "":
			return fmt.Sprintf("An attached file (%s) was provided for this turn.", mediaType)
		default:
			return "An attached file was provided for this turn."
		}
	}
	switch {
	case name != "" && mediaType != "":
		return fmt.Sprintf("Attached file %q (%s) is available at:\n%s", name, mediaType, uri)
	case name != "":
		return fmt.Sprintf("Attached file %q is available at:\n%s", name, uri)
	case mediaType != "":
		return fmt.Sprintf("An attached file (%s) is available at:\n%s", mediaType, uri)
	default:
		return "An attached file is available at:\n" + uri
	}
}

func RequirePromptText(params runtimeacp.PromptParams) (string, error) {
	text, err := renderPreparedPrompt(params)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New("prompt text is required")
	}
	return text, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneMCPServers(values []runtimeacp.MCPServer) []runtimeacp.MCPServer {
	if values == nil {
		return nil
	}
	out := make([]runtimeacp.MCPServer, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Args = append([]string(nil), value.Args...)
		out[i].Env = append([]runtimeacp.EnvVariable(nil), value.Env...)
		out[i].Headers = append([]runtimeacp.HTTPHeader(nil), value.Headers...)
		if value.Meta != nil {
			out[i].Meta = make(map[string]any, len(value.Meta))
			for key, metaValue := range value.Meta {
				out[i].Meta[key] = metaValue
			}
		}
	}
	return out
}

func cloneTranscript(values []transcriptExchange) []transcriptExchange {
	if values == nil {
		return nil
	}
	return append([]transcriptExchange(nil), values...)
}

func firstNonEmpty(first, fallback string) string {
	if strings.TrimSpace(first) != "" {
		return first
	}
	return fallback
}

func firstNonNil[T any](first, fallback []T) []T {
	if first != nil {
		return first
	}
	return fallback
}
