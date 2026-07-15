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
  events before continuing (with cancellation abandoning the pending request,
  best-effort notifying through a bounded writer queue, and discarding any late
  client response), optionally mapping provider-specific missing native
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
  media type, and safe non-local URI metadata; local `file:` identifiers are
  omitted;
- non-file resource links remain ordered references and are never fetched by
  the kit.

Only absolute local URIs that resolve directly to non-symlink regular files are
accepted. Traversal segments, remote `file:` hosts, directories, devices,
symlinks, and reparse points fail closed. Rich file preparation is supported on
Darwin, Linux, and Windows. On Darwin and Linux, every canonical temporary-parent
ancestor must be owned by the process user or root; a directory writable by
another principal must be sticky and extended/POSIX ACLs must be absent. The
mount must also be local: Darwin requires `MNT_LOCAL`, while Linux accepts only
ext-family, XFS, Btrfs, tmpfs, overlay, ramfs, and F2FS filesystem types. NFS,
SMB/CIFS, FUSE, and all other filesystems—including ZFS, AUFS, eCryptfs, and
9p—fail closed even when their POSIX ACL xattrs appear absent. Configure
`Spec.PromptResourceTempDir` on an allowlisted local filesystem when the default
temporary directory does not qualify. The stage is private while being
populated (`0700`), its files are read-only (`0400`), and the completed
directory is read/execute-only (`0500`) while the command runs. Inherited ACLs
are stripped from the private stage and files and then verified through retained
handles. On Windows, the temporary parent must be on a local drive and every
ancestor must be a non-reparse directory with a trusted owner. Retained ancestor
handles deny delete sharing, and validation rejects untrusted grants of direct
delete, delete-child, DACL/owner mutation, or full-control rights. Preparation
atomically creates each private directory with a protected, inheritable DACL
containing only the current process user and SYSTEM; the kit reads the directory
DACL back, verifies every new child's inherited DACL before writing bytes, and
fails closed when the filesystem cannot enforce either. Other platforms reject
rich file preparation rather than relying on weaker filesystem semantics.

The cloned `Session` passed to `BuildPrompt` contains the stage as one
additional directory; persistent session state never does. Exact stage cleanup
is attempted after success, builder or process failure, and cancellation.
Cleanup uses retained directory identities and deletes only exact stage
entries; it does not recursively delete through a re-resolved stage path.
Cleanup must succeed before prompt counts or transcript state are updated. The
kit makes a bounded retry when exact cleanup fails; a persistent failure
changes the prompt result to an error, closes every retained identity handle,
and leaves any protected remnant for manual removal after the adapter process
exits. These controls limit inherited or untrusted-principal access
and accidental path replacement; they are not sandbox isolation from another
process running as the adapter's OS user, which can inspect a discovered path
or change owner-controlled modes and DACLs. Exact private paths and file URIs
are scrubbed from builder errors, command output, tool activity, final RPC
errors, and recorded transcripts, including ordinary stream chunk boundaries.
This is path-alias hygiene, not DLP against the selected agent deliberately
transforming or segmenting an alias: short or pathless fragments are not
guaranteed to be removed. User-provided attachment display names and standalone
display-name-derived staged filenames remain intentional prompt metadata.

Preparation defaults to at most 4 files, 5 MiB per file, and 12 MiB total.
Adapters can lower or raise those provider-neutral bounds with
`Spec.PromptResourceLimits` and can select the parent directory with
`Spec.PromptResourceTempDir`, which must be an absolute trusted local directory
meeting the platform rules above when set.

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

`runtimeacp.EmbeddedResource.Kind` preserves whether an empty required ACP
union value was `text` or `blob`; non-empty legacy struct literals continue to
infer the variant. Because this adds an exported field to a public struct, the
next compatible release containing rich prompt inputs is `v0.2.0`.

Keep provider-specific command arguments, model lists, reasoning options, and
auth guidance in the adapter repositories.

## Verification

Run the kit test suite with:

```sh
go test ./...
go vet ./...
go test -race ./...
```

CI runs the complete suite natively on macOS and Windows as well as the Linux
formatting, vet, test, and race jobs. This exercises Darwin ACL removal and
readback plus Windows DACL, reparse-point, and exact-handle cleanup behavior on
their native platforms.
