# Changelog

## [Unreleased]

### Fixed

- Bound waiting for the diagnostic rotation lock at startup and during writes.

### Added

- Shared Unix diagnostics with a private, redacting `slog.Handler` and
  bounded, multi-process-safe rotation.
- Schema-validated OpenTelemetry with private SQLite, bounded retention,
  salted identifiers, off switches and consent-gated OTLP/HTTP.
- An installable example and an isolated-home smoke test.
