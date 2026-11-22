package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSchemas = []Schema{
	{Name: "sample.cli.run", Version: 1, Signal: Span, Fields: map[string]Field{
		"outcome": {Kind: Enum, Members: []EnumMember{"ok", "failed"}, Required: true},
		"index":   {Kind: Integer},
		"local":   {Kind: Boolean},
		"subject": {Kind: Identifier},
	}},
	{Name: "sample.cli.finish", Version: 1, Signal: Event},
	{Name: "sample.cli.count", Version: 1, Signal: Counter, Unit: UnitOne},
	{Name: "sample.cli.elapsed", Version: 1, Signal: Histogram, Unit: UnitMilliseconds},
}

func recordExample(c *Client) {
	c.Record(Observation{Name: "sample.cli.run", Version: 1, Duration: time.Millisecond,
		Attributes: []Value{EnumValue("outcome", "ok"), Int("index", 3), Bool("local", true), HashIdentifier("subject", "fixture-person@example.invalid")}})
	c.Record(Observation{Name: "sample.cli.finish", Version: 1})
	c.Record(Observation{Name: "sample.cli.count", Version: 1, Amount: 3})
	c.Record(Observation{Name: "sample.cli.elapsed", Version: 1, Amount: 23})
}

func TestSQLiteCapturesOnlyValidatedOTelSignals(t *testing.T) {
	home := freshHome(t)
	c := New("sample", Config{}, testSchemas...)
	waitReady(t, c)
	recordExample(c)
	for _, input := range []Observation{
		{Name: "sample.cli.run", Version: 7, Attributes: []Value{EnumValue("outcome", "ok")}},
		{Name: "sample.cli.run", Version: 1, Attributes: []Value{EnumValue("outcome", "arbitrary-text")}},
		{Name: "sample.cli.run", Version: 1, Attributes: []Value{Int("outcome", 1)}},
		{Name: "sample.cli.run", Version: 1, Attributes: []Value{EnumValue("outcome", "ok"), Int("not.allowed", 42)}},
		{Name: "sample.cli.run", Version: 1},
	} {
		c.Record(input)
	}
	c.Shutdown(t.Context())
	path := sqlitePath(home)
	db := connectTest(t, path)
	rows, err := db.QueryContext(t.Context(), "SELECT signal, schema_version, attributes, value FROM signals ORDER BY signal")
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var signals []string
	for rows.Next() {
		var name, encoded string
		var schema, value int
		require.NoError(t, rows.Scan(&name, &schema, &encoded, &value))
		assert.Equal(t, 1, schema)
		assert.NotContains(t, encoded, "fixture-person")
		if name == "span" {
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(encoded), &fields))
			assert.Equal(t, true, fields["local"])
			assert.Equal(t, float64(3), fields["index"])
			assert.Equal(t, "ok", fields["outcome"])
			assert.Regexp(t, `^[0-9a-f]{64}$`, fields["subject"])
		}
		if name == "counter" {
			assert.Equal(t, 3, value)
		}
		signals = append(signals, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"counter", "event", "histogram", "span"}, signals)
	assertMode(t, filepath.Dir(path), 0o700)
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Join(home, ".punt-labs", saltName), 0o600)
}

func TestOTLPConsentAndOverrides(t *testing.T) {
	var trace, metrics atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		assert.NoError(t, err)
		assert.Equal(t, "", req.Header.Get("X-Inherited-Secret"))
		assert.NotContains(t, string(body), "fixture-person")
		assert.NotContains(t, string(body), "fixture-host")
		assert.Contains(t, string(body), "sample")
		switch req.URL.Path {
		case "/v1/traces":
			assert.Contains(t, string(body), "sample.cli.finish")
			trace.Add(1)
		case "/v1/metrics":
			assert.Contains(t, string(body), "sample.cli.count")
			metrics.Add(1)
		default:
			t.Errorf("unexpected OTLP path: %s", req.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	home := freshHome(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.name=fixture-host")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	t.Setenv("OTEL_SPAN_EVENT_COUNT_LIMIT", "0")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-inherited-secret=fixture-secret")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	local := New("sample", Config{OTLPEndpoint: server.URL}, testSchemas...)
	waitReady(t, local)
	recordExample(local)
	local.Shutdown(t.Context())
	require.FileExists(t, sqlitePath(home))
	assert.Zero(t, trace.Load())
	assert.Zero(t, metrics.Load())
	require.NoError(t, os.Remove(sqlitePath(home)))

	network := New("sample", Config{OTLPEndpoint: server.URL, OTLPConsent: true}, testSchemas...)
	waitReady(t, network)
	recordExample(network)
	network.Shutdown(t.Context())
	assert.Equal(t, int32(1), trace.Load())
	assert.Equal(t, int32(1), metrics.Load())
	assert.NoFileExists(t, sqlitePath(home))
}

func TestAnyOffSwitchWins(t *testing.T) {
	for _, tc := range []struct {
		name, global, tool string
		disabled           bool
	}{
		{"global", "off", "on", false},
		{"tool", "on", "off", false},
		{"config", "on", "on", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := freshHome(t)
			t.Setenv("PUNT_TELEMETRY", tc.global)
			t.Setenv("SAMPLE_TELEMETRY", tc.tool)
			c := New("sample", Config{Disabled: tc.disabled, OTLPConsent: true, OTLPEndpoint: "https://example.invalid"}, testSchemas...)
			waitReady(t, c)
			recordExample(c)
			c.Shutdown(t.Context())
			assert.Nil(t, c.queue)
			assert.NoDirExists(t, filepath.Join(home, ".punt-labs"))
		})
	}
}

func TestFailureIsolationAndQueueSaturation(t *testing.T) {
	freshHome(t)
	var notices atomic.Int32
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		notices.Add(1)
		assert.Contains(t, string(p), "Telemetry degraded")
		assert.NotContains(t, string(p), "http://")
		return len(p), nil
	}), nil))
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		once.Do(func() { close(entered) })
		select {
		case <-req.Context().Done():
		case <-time.After(4 * time.Second):
		}
	}))
	defer server.Close()
	c := New("sample", Config{OTLPEndpoint: server.URL, OTLPConsent: true, Diagnostics: log}, testSchemas...)
	waitReady(t, c)
	recordExample(c)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("export was not attempted")
	}
	start := time.Now()
	for range 10000 {
		c.Record(Observation{Name: "sample.cli.finish", Version: 1})
	}
	assert.Less(t, time.Since(start), time.Second)
	assert.LessOrEqual(t, len(c.queue), queueCapacity)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c.Shutdown(ctx)
	require.NoError(t, ctx.Err())
	assert.Eventually(t, func() bool { return notices.Load() == 1 }, time.Second, time.Millisecond)
}

func TestUnwritableStoreDoesNotFailOperation(t *testing.T) {
	home := freshHome(t)
	require.NoError(t, os.MkdirAll(sqlitePath(home), 0o700))
	output := make(chan string, 1)
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		output <- string(p)
		return len(p), nil
	}), nil))
	c := New("sample", Config{Diagnostics: log}, testSchemas...)
	recordExample(c)
	c.Shutdown(t.Context())
	select {
	case message := <-output:
		assert.Contains(t, message, "initialization failed")
		assert.NotContains(t, message, home)
	case <-time.After(time.Second):
		t.Fatal("missing diagnostic")
	}
}

func TestBlockedDiagnosticsDoNotBlockWorker(t *testing.T) {
	freshHome(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		close(entered)
		<-release
		return len(p), nil
	}), nil))
	c := New("sample", Config{RetentionDays: -1, Diagnostics: log}, testSchemas...)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("diagnostic not attempted")
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("diagnostic blocked the telemetry worker")
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func freshHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PUNT_TELEMETRY", "")
	t.Setenv("SAMPLE_TELEMETRY", "")
	return home
}

func waitReady(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("telemetry did not become ready")
	}
}

func sqlitePath(home string) string { return filepath.Join(home, ".punt-labs", "sample", databaseName) }

func connectTest(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(400)")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	return db
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode().Perm(), path)
}
