# ACP Adapter Kit

Go building blocks for ACP adapters that wrap external coding-agent CLIs.

This module intentionally contains provider-neutral pieces only:

- local ACP JSON-RPC server transport;
- provider-neutral Cobra command scaffolding for ACP adapters;
- command-backed ACP session scaffolding for adapters that invoke a local CLI
  per prompt;
- runtime JSON-RPC client and cancellation plumbing;
- ACP runtime request/result helpers;
- runtime bridge and host scaffolding;
- process launching and bounded output capture;
- provider-neutral doctor command runner support;
- fake runtime and protocol test utilities.
- reusable adapter conformance assertions for provider-neutral initialize,
  capability, auth, session selector, and available-command checks.

Adapter repositories should keep provider-specific behavior local, including
binary names, capabilities, environment allowlists, doctor wording, launch
defaults, README copy, release workflows, and vendor-specific ACP quirks.

Process-backed runtime launches use explicit environment policies. Adapter
repos should pass only the provider-specific runtime variables they intend the
child runtime to see; the kit does not inherit the parent environment by
default.

Adapters have two runtime integration paths:

- `runtimehost` / `runtimebridge` proxy an explicit child ACP runtime process.
- `commandbridge` owns lightweight ACP sessions in Go and invokes a configured
  local command for each prompt, emitting a generic `tool_call` activity around
  the command, forwarding stdout as assistant text, cancelling the process when
  ACP `session/cancel` arrives, supporting in-memory `session/load`,
  `session/resume`, and `session/fork`, returning command-backed session list
  metadata, publishing `config_option_update` notifications after config
  changes, publishing `session_info_update` notifications when transcript
  metadata changes, translating structured command streams into ACP updates
  when a parser is configured, requesting ACP tool permissions from parsed
  stream events before continuing, and optionally prepending a bounded
  transcript prelude to later prompt commands. Adapters for CLIs with native
  session ids may opt into adopting unknown `session/load` or `session/resume`
  ids so the provider command can continue a session known to the host after an
  adapter process restart. Command-backed adapters can also advertise ACP
  `authMethods`, run a fixed-argv native login command for `authenticate`, and
  advertise/run ACP `logout` when the provider CLI supports ending local auth.

Keep provider-specific command arguments, model lists, reasoning options, and
auth guidance in the adapter repositories.

## Verification

Run the kit test suite with:

```sh
go test ./...
```
