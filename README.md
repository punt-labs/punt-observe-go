# punt-observe-go

> Private diagnostic logs and local OpenTelemetry for Go tools.

[![Go Reference](https://pkg.go.dev/badge/github.com/punt-labs/punt-observe-go.svg)](https://pkg.go.dev/github.com/punt-labs/punt-observe-go)
[![Test](https://github.com/punt-labs/punt-observe-go/actions/workflows/test.yml/badge.svg)](https://github.com/punt-labs/punt-observe-go/actions/workflows/test.yml)

This is the Go counterpart of [punt-observe](https://github.com/punt-labs/punt-observe).
It supplies a private, rotating `log/slog` sink and a separate, schema-validated
OpenTelemetry API for CLI tools, servers, hooks and workers. Telemetry stays on
the machine by default; sending it over the network requires explicit consent.

**Platforms:** Linux, macOS · Go 1.26+

## Quick Start

Run the example in an isolated home:

```sh
scratch="$(mktemp -d)"
mkdir -p "$scratch/bin" "$scratch/home"
GOBIN="$scratch/bin" go install ./cmd/punt-observe-smoke
HOME="$scratch/home" "$scratch/bin/punt-observe-smoke" -spawn
ls "$scratch/home/.punt-labs/punt-observe-smoke"
```

From a clone, run `make smoke` to install the example into a test directory,
drive three concurrent processes through rotation, and inspect the logs and
SQLite data, including a detached worker with discarded standard streams.

## Features

- Private diagnostic files shared safely by separate processes, with five
  backups and a hard 5 MiB bound per active or rotated file.
- Full-record redaction and one physical line per diagnostic record.
  Applications must still keep user content and credentials out of log calls.
- Typed telemetry schemas, local SQLite with bounded retention, optional
  OTLP/HTTP, and a bounded non-blocking observation queue.

## Using the Package

In each process entry point, create a handler with
`logging.New("my-tool", "cli")`, handle initialization errors, and pass
`slog.New(handler).With("component", "my-tool.cli")` to the code doing work.
Close the handler after those loggers are done. Configure detached workers at
their own entry points; do not use stderr as a durable sink or write logs to
MCP stdout.

Declare versioned `telemetry.Schema` values at build time. Use
`telemetry.New` with the tool name, configuration, and schema declarations.
Use `Client.Record` with the declared name and version and the typed field
constructors. `HashIdentifier` hands an identifier to the shared API for
derivation; do not supply plain identifiers through any other field type.
Observations with identifier fields may be dropped during asynchronous startup.
Use a short, bounded context to request a best-effort `Shutdown`.

| Control | Effect |
| ------- | ------ |
| Default | Private SQLite capture, 90-day retention |
| `Config.RetentionDays` | Set retention to 1–3650 days |
| `PUNT_TELEMETRY=off` | Disable all capture and export |
| `<TOOL>_TELEMETRY=off` | Disable capture and export for one tool (uppercase; hyphens become underscores) |
| `Config.Disabled` | Disable capture and export from tool configuration |
| `Config.OTLPConsent` and `Config.OTLPEndpoint` | Swap SQLite for OTLP/HTTP without changing instrumentation |

Set the endpoint to an HTTPS base URL; plaintext HTTP is allowed only for a
literal loopback IP. The SDK appends `/v1/traces` and `/v1/metrics` to that
base. A generic OTel environment variable cannot grant consent or select a
destination. Any off switch wins.

## Documentation

- [Design](DESIGN.md)
- [Changelog](CHANGELOG.md)
- [Punt Labs Go Logging and Telemetry Rules](https://github.com/punt-labs/punt-kit/blob/main/lang-rules/go/go-logging.md)

## License

[MIT](LICENSE)
