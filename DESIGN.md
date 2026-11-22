# Design

## Public Packages, Private Implementations

`logging` and `telemetry` are the importable boundaries; Unix filesystem
operations and the example's process wiring are internal. No provider is
installed globally. The same tool can record through local or network
exporters without changing its instrumentation.

## Diagnostic Files

The Unix file sink uses an owner-only directory and a stable lock file. It
uses a mutex within a handler and `flock` across processes. It opens the
active file under the lock for each append, so it never caches an inode
across rollover. Ownership, type and mode are checked before use. Backups
are renamed under the same lock. This requires a filesystem that honors
local Unix file locking; replace the sink with a single owner for other
topologies.

The handler formats attributes, groups and error values before running
bounded redaction passes. Arbitrary composite values are not traversed;
use typed attributes, groups or a `slog.LogValuer` for structured metadata.
It masks whole URL tokens rather than preserving
authorities, as authorities may also be sensitive, and withholds oversized
or unsettled records. Shared escaping encodes control and format runes;
only the sink appends the line terminator. Durable write failure emits a
fixed, rate-limited stderr notice rather than calling a logger recursively.

## Telemetry

Schemas are trusted declarations, not runtime input. Call sites use typed
constructors with runtime validation against the sealed declaration copy.
The queue contains only validated attributes and trusted signal metadata.
The per-install 32-byte random secret is published once under an
inter-process lock. HMAC-SHA-256 derivation happens before observations are
enqueued; no raw identifiers are retained. Only the validated tool name is
exported as resource metadata; ambient SDK resource attributes are replaced
before handing signals to either exporter.

One worker owns isolated OpenTelemetry Go trace and metric providers.
Standalone events are SDK span events on a dedicated instrumentation scope;
the local exporter persists the event rather than its envelope span.
Metrics are collected with delta temporality. The SQLite exporter
transactionally writes signal identity, schema version, timestamps, typed
JSON attributes and metric aggregation data. SQLite's rollback journal
uses the database's private mode. A process lock serializes module writers
and maintenance within the I/O deadline; SQLite's busy timeout also bounds
contention with external connections. Incremental vacuum reclaims expired pages.

The worker bounds export and shutdown I/O, and drops instead of waiting on
saturation. The OTLP exporter has no retries, redirects, inherited proxy or
ambient auth headers. Local database failures and network failures are
best-effort diagnostics, not tool failures. A client permits only one
diagnostic write in flight; a blocked diagnostic logger cannot block capture.
