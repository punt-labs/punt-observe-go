package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	_ "modernc.org/sqlite"
)

const databaseName = "telemetry.db"
const ddl = `
CREATE TABLE IF NOT EXISTS signals (
 signal TEXT NOT NULL, name TEXT NOT NULL, schema_version INTEGER NOT NULL,
 start_ns INTEGER NOT NULL, end_ns INTEGER NOT NULL, trace_id TEXT, span_id TEXT,
 unit TEXT, value INTEGER, count INTEGER, attributes TEXT NOT NULL, aggregation TEXT
);
CREATE INDEX IF NOT EXISTS signals_expiry ON signals(end_ns);
PRAGMA user_version=1;
`
const insertSignal = `INSERT INTO signals VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

type sqliteExporter struct {
	db   *sql.DB
	lock *os.File
	days int
}

func openSQLite(ctx context.Context, tool string, days int) (_ *sqliteExporter, err error) {
	dir, err := privatefs.Open(tool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	lock, err := dir.FileWith(".telemetry.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	if err := privatefs.LockContext(ctx, lock); err != nil {
		return nil, err
	}
	if err := prepareDatabase(dir); err != nil {
		return nil, err
	}
	address := url.URL{Scheme: "file", Path: filepath.Join(dir.Path, databaseName), RawQuery: "_pragma=busy_timeout%28200%29&mode=rw"}
	db, err := sql.Open("sqlite", address.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	store := &sqliteExporter{db: db, lock: lock, days: days}
	if err := store.setup(ctx); err != nil {
		return nil, err
	}
	if err := store.pruneRows(ctx); err != nil {
		return nil, err
	}
	if err := privatefs.Unlock(lock); err != nil {
		return nil, err
	}
	return store, nil
}

func prepareDatabase(dir *privatefs.Dir) error {
	file, err := dir.FileWith(databaseName, os.O_CREATE|os.O_EXCL|os.O_RDWR)
	if err == nil {
		if err := file.Close(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	return dir.Tighten(databaseName, databaseName+"-journal", databaseName+"-wal", databaseName+"-shm")
}

func (s *sqliteExporter) setup(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch version {
	case 1:
		return nil
	case 0:
		if _, err := s.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return err
		}
		return transaction(ctx, s.db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, ddl)
			return err
		})
	default:
		return errors.New("unknown local telemetry store version")
	}
}

func transaction(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqliteExporter) prune(ctx context.Context) error {
	return s.withLock(ctx, s.pruneRows)
}

func (s *sqliteExporter) withLock(ctx context.Context, action func(context.Context) error) (err error) {
	if err := privatefs.LockContext(ctx, s.lock); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, privatefs.Unlock(s.lock)) }()
	return action(ctx)
}

func (s *sqliteExporter) pruneRows(ctx context.Context) error {
	cutoff := time.Now().AddDate(0, 0, -s.days).UnixNano()
	for {
		res, err := s.db.ExecContext(ctx, "DELETE FROM signals WHERE rowid IN (SELECT rowid FROM signals WHERE end_ns < ? ORDER BY end_ns LIMIT 512)", cutoff)
		if err != nil {
			return err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if count < 512 {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum(256)")
	return err
}

func (s *sqliteExporter) close(context.Context) error {
	return errors.Join(s.db.Close(), s.lock.Close())
}

func (s *sqliteExporter) write(ctx context.Context, action func(*sql.Tx) error) error {
	return s.withLock(ctx, func(ctx context.Context) error {
		return transaction(ctx, s.db, action)
	})
}

type signalRow struct {
	kind        string
	name        string
	start       time.Time
	end         time.Time
	traceID     string
	spanID      string
	unit        string
	value       int64
	count       uint64
	attrs       []attribute.KeyValue
	aggregation any
}

func (r signalRow) insert(ctx context.Context, tx *sql.Tx) error {
	attrs := make(map[string]any, len(r.attrs))
	var version int64
	for _, a := range r.attrs {
		if a.Key == "punt.schema.version" {
			version = a.Value.AsInt64()
		} else {
			attrs[string(a.Key)] = a.Value.AsInterface()
		}
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	var aggregation string
	if r.aggregation != nil {
		encodedAggregation, err := json.Marshal(r.aggregation)
		if err != nil {
			return err
		}
		aggregation = string(encodedAggregation)
	}
	_, err = tx.ExecContext(ctx, insertSignal, r.kind, r.name, version, r.start.UnixNano(),
		r.end.UnixNano(), r.traceID, r.spanID, r.unit, r.value, r.count, string(encoded), aggregation)
	return err
}

func (s *sqliteExporter) exportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, span := range spans {
			id := span.SpanContext()
			base := signalRow{traceID: id.TraceID().String(), spanID: id.SpanID().String()}
			if span.InstrumentationScope().Name != eventScope {
				row := base
				row.kind, row.name = "span", span.Name()
				row.start, row.end, row.attrs = span.StartTime(), span.EndTime(), span.Attributes()
				if err := row.insert(ctx, tx); err != nil {
					return err
				}
			}
			for _, event := range span.Events() {
				row := base
				row.kind, row.name = "event", event.Name
				row.start, row.end, row.attrs = event.Time, event.Time, event.Attributes
				if err := row.insert(ctx, tx); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *sqliteExporter) exportMetrics(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, scope := range metrics.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if err := persistMetric(ctx, tx, metric); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func persistMetric(ctx context.Context, tx *sql.Tx, metric metricdata.Metrics) error {
	base := signalRow{name: metric.Name, unit: metric.Unit}
	switch data := metric.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			row := base
			row.kind, row.value = "counter", point.Value
			row.start, row.end, row.attrs = point.StartTime, point.Time, point.Attributes.ToSlice()
			if err := row.insert(ctx, tx); err != nil {
				return err
			}
		}
	case metricdata.Histogram[int64]:
		for _, point := range data.DataPoints {
			row := base
			row.kind, row.value, row.count = "histogram", point.Sum, point.Count
			row.start, row.end, row.attrs = point.StartTime, point.Time, point.Attributes.ToSlice()
			row.aggregation = struct {
				Bounds  []float64 `json:"bounds"`
				Buckets []uint64  `json:"buckets"`
			}{point.Bounds, point.BucketCounts}
			if err := row.insert(ctx, tx); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected telemetry metric data")
	}
	return nil
}
