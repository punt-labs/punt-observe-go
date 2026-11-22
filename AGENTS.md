# Developing punt-observe-go

Keep diagnostics and telemetry independent. Never log customer content or
record rejected telemetry values. Only the shared module owns file
preparation, rotation, schema validation, and exporter configuration.
Do not install global OTel providers or alter the application's default logger.

Use `make check` for lint, Markdown and race-enabled tests, and `make smoke`
to exercise the installed example. Keep scratch files under ignored `.tmp/`.
Track changes in Linear (team PL); repository adoption is tracked separately.
