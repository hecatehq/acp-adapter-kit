# ACP Adapter Kit

This repository contains provider-neutral Go building blocks for ACP adapter
repositories.

Keep this repo free of Codex-, Claude-, Hecate-, or product-specific defaults.
Provider names, default binaries, environment allowlists, doctor wording,
release workflows, and vendor-specific ACP quirks belong in the adapter repos.

Shared protocol/runtime/CLI plumbing belongs here:

- local stdio ACP transport and JSON-RPC framing;
- typed runtime ACP helpers;
- runtime bridge, host, and process launch scaffolding;
- doctor runner/report plumbing;
- provider-neutral adapter CLI scaffolding;
- fake runtime and protocol test utilities.

Do not replace the local stdio ACP transport with an SDK transport unless these
invariants remain covered by tests: ordered method dispatch, concurrent
cancel/notification behavior, JSON-RPC error handling, distinct outbound request
IDs, and the 1 MiB message cap.

For command-backed rich file inputs, keep staging provider-neutral and
fail-closed. Darwin, Linux, and Windows are the supported security backends.
Preserve full ancestor validation, retained path identities, private
owner/mode-or-DACL enforcement, trusted local-filesystem checks, pre-builder and
pre-launch verification, and exact handle-relative cleanup. Do not replace
exact cleanup with a recursive delete through a re-resolved path, broaden the
Linux filesystem allowlist without a security review, or expose source-local
file URIs or ephemeral stage paths in errors, activity, output, or transcripts.

When changing shared behavior, add or update focused kit tests first, then run:

```sh
go test ./...
go vet ./...
go test -race ./...
```
