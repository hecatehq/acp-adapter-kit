# ACP Adapter Kit

Go building blocks for ACP adapters that wrap external coding-agent CLIs.

This module intentionally contains provider-neutral pieces only:

- local ACP JSON-RPC server transport;
- provider-neutral Cobra command scaffolding for ACP adapters;
- command-backed ACP session scaffolding for adapters that invoke a local CLI
  per prompt;
- runtime JSON-RPC client and cancellation plumbing;
- ACP runtime request/result helpers for sessions, config, MCP, auth, and
  terminal callbacks;
- runtime bridge and host scaffolding;
- process launching and bounded output capture;
- provider-neutral doctor command runner support;
- fake runtime and protocol test utilities;
- reusable live ACP test client utilities for opt-in integration smoke tests,
  including auto-allow, auto-reject, and auto-cancel permission responses;
- reusable stdio MCP echo-server fixture for adapter smoke tests;
- reusable adapter conformance assertions for provider-neutral initialize,
  capability, auth, session selector, and available-command checks.

Adapter repositories should keep provider-specific behavior local, including
binary names, capabilities, environment allowlists, doctor wording, launch
defaults, README copy, release workflows, and vendor-specific ACP quirks.

Process-backed runtime launches use explicit environment policies. Adapter
repos should pass only the provider-specific runtime variables they intend the
child runtime to see; the kit does not inherit the parent environment by
default.

Embedded hosts can construct `commandbridge.ProcessRunner` with a host-owned
base environment. That constructor always treats its input as authoritative;
nil or empty means inherit nothing, so provider subprocesses cannot recover
secrets from the host process. At the lower-level process API, a nil base keeps
the standalone adapter's normal system-environment behavior.

Adapters have two runtime integration paths:

- `runtimehost` / `runtimebridge` proxy an explicit child ACP runtime process.
- `commandbridge` owns lightweight ACP sessions in Go and invokes a configured
  local command for each prompt, emitting a generic `tool_call` activity around
  the command, forwarding stdout as assistant text, cancelling the process when
  ACP `session/cancel` arrives, dropping stream chunks delivered after
  cancellation, supporting in-memory `session/load`, `session/resume`, and
  `session/fork`, returning command-backed session list metadata, treating ACP
  `session/close` as active-work cancellation plus in-memory session cleanup,
  publishing `config_option_update` notifications after config changes,
  publishing `session_info_update` notifications when transcript metadata
  changes, translating structured command streams into ACP updates when a
  parser is configured, requesting ACP tool permissions from parsed stream
  events before continuing, optionally mapping provider-specific missing native
  conversation failures to a typed `native_session_missing` prompt-error
  discriminator, and optionally prepending a bounded transcript prelude to
  later prompt commands. Prompt command arguments are redacted from ACP tool
  activity so user prompts, attachment names, and prompt-scoped paths are not
  persisted as `rawInput`.
  The host decides whether replacing that missing native conversation is safe;
  the kit never retries a prompt. The classifier runs only for a non-zero
  process exit; adapters must also reject partial or truncated output and bind
  provider-specific failures to the exact native session. Adapters for CLIs
  with native session ids may opt into adopting unknown `session/load` or
  `session/resume` ids so the provider command can continue a session known to
  the host after an adapter process restart. Command-backed adapters can also
  advertise ACP
  `session/delete` as destructive in-memory session cleanup, advertise ACP
  `authMethods`, run a fixed-argv native login command for `authenticate`, and
  advertise/run ACP `logout` when the provider CLI supports ending local auth.
  Command subprocesses run in an owned Unix process group or Windows
  kill-on-close Job Object, so cancellation terminates descendants before the
  runtime returns; inherited pipe drain is bounded on every platform.
  Resource-link prompt blocks are rendered as explicit attachment name, MIME
  type, and URI text so command-backed CLIs can consume host-staged file and
  image paths without claiming unsupported inline ACP content. Bounded
  transcripts retain only attachment metadata; ephemeral paths are removed
  from both user and assistant history and never replayed after the originating
  command.

### Command-backed rich prompt inputs

Before `commandbridge` invokes `BuildPrompt`, it prepares ACP rich inputs in a
private directory created for that prompt:

- local `file:` resource links are copied without granting the command access
  to the source parent directory;
- base64 image/audio data and embedded-resource blobs are decoded into private
  local files;
- embedded-resource text remains inline and is labeled with JSON-escaped name,
  media type, and URI metadata;
- non-file resource links remain ordered references and are never fetched by
  the kit.

Only absolute local URIs that resolve directly to non-symlink regular files are
accepted. Traversal segments, remote `file:` hosts, directories, devices, and
symlinks and reparse points fail closed. On Darwin, DragonFly BSD, FreeBSD,
Linux, NetBSD, and OpenBSD, the stage is private while being populated (`0700`),
its files are read-only (`0400`), and the completed directory is
read/execute-only (`0500`) while the command runs. On Windows, preparation
replaces inherited permissions before writing any bytes with a protected,
inheritable DACL containing only the current process user and SYSTEM; the kit
reads the directory DACL back, verifies every new child's inherited DACL before
writing bytes, and fails closed when the filesystem cannot enforce either.
Other platforms reject rich file preparation rather than relying on weaker
filesystem semantics.

The cloned `Session` passed to `BuildPrompt` contains the stage as one
additional directory; persistent session state never does. The stage is
removed after success, builder or process failure, and cancellation. Cleanup
must succeed before prompt counts or transcript state are updated, and a
cleanup failure changes the prompt result to an error. Exact private paths,
file URIs, stage names, and staged filenames are scrubbed from builder errors,
command output, tool activity, final RPC errors, and recorded transcripts.

Preparation defaults to at most 4 files, 5 MiB per file, and 12 MiB total.
Adapters can lower or raise those provider-neutral bounds with
`Spec.PromptResourceLimits` and can select the parent directory with
`Spec.PromptResourceTempDir`, which must be absolute when set.

Prompt builders can call `commandbridge.PreparedPromptInputs` for an ordered,
typed view with exact private paths and preserved link metadata when a provider
needs flags such as an image argument. Materialized images and embedded blobs
also expose safe non-local source metadata as `OriginalURI`; source-local file
URIs are never exposed. `commandbridge.RequirePromptText`
renders a fixed, JSON-escaped manifest for command-backed CLIs and returns an
actionable error for any raw, unprepared, malformed, or unsupported block. The
legacy `PromptText` helper returns an empty string on that validation failure
and should not be used when the builder can propagate an error. When transcript
inclusion is enabled, a history-only prelude is inserted before the current
turn while every current text and rich block remains in its original order.
Only ordinary ACP text blocks enter history; attachment bodies and
rich-resource text are excluded.

Keep provider-specific command arguments, model lists, reasoning options, and
auth guidance in the adapter repositories.

## Verification

Run the kit test suite with:

```sh
go test ./...
go vet ./...
go test -race ./...
```
