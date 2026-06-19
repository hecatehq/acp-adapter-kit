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
  local command for each prompt, forwarding stdout as assistant text and
  cancelling the process when ACP `session/cancel` arrives.

Keep provider-specific command arguments, model lists, reasoning options, and
auth guidance in the adapter repositories.

## Verification

Run the kit test suite with:

```sh
go test ./...
```
