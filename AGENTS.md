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

When changing shared behavior, add or update focused kit tests first, then run:

```sh
go test ./...
go vet ./...
go test -race ./...
```
