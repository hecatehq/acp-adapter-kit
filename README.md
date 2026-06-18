# ACP Adapter Kit

Go building blocks for ACP adapters that wrap external coding-agent CLIs.

This module intentionally contains provider-neutral pieces only:

- local ACP JSON-RPC server transport;
- runtime JSON-RPC client and cancellation plumbing;
- ACP runtime request/result helpers;
- runtime bridge and host scaffolding;
- process launching and bounded output capture;
- provider-neutral doctor command runner support;
- fake runtime and protocol test utilities.

Adapter repositories should keep provider-specific behavior local, including
binary names, environment allowlists, doctor wording, launch defaults, README
copy, release workflows, and vendor-specific ACP quirks.

## Verification

Run the kit test suite with:

```sh
go test ./...
```
