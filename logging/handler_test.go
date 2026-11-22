package logging

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nestedValue struct{}

func (nestedValue) LogValue() slog.Value {
	return slog.GroupValue(slog.String("url", "https://fixture:fixture-pass@private.invalid/path-secret?q=query-secret#fragment-secret"))
}

func TestPrivateDiagnosticHandler(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".punt-labs", "sample", "logs")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "sample.log")
	for _, name := range []string{path, path + ".1", path + ".rotate.lock"} {
		require.NoError(t, os.WriteFile(name, nil, 0o644))
	}
	t.Chdir(t.TempDir())
	h, err := New("sample", "cli")
	require.NoError(t, err)
	defer func() { assert.NoError(t, h.Close()) }()
	log := slog.New(h).With("component", "sample.worker").WithGroup("details").With("bound", nestedValue{})
	log.Debug("not visible")
	log.Warn("Rejected malformed request", "error", errors.New("Bearer fixture-auth\nforged\x1b[1m\u0085\u2028\u2029"), "cookie", "fixture cookie secret", "db_password", "fixture multiword", "payload", []string{"fixture composite"})
	log.Info(strings.Repeat("oversized ", maxRecordBytes))
	log.Info("oversized binary", "stack", make([]byte, maxRecordBytes))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(data)
	assert.Regexp(t, `^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \[WARNING\] sample\.worker: Rejected malformed request .+\n$`, text)
	assert.Equal(t, 1, strings.Count(text, "\n"))
	assert.Contains(t, text, `\x0aforged\x1b[1m\x85\u2028\u2029`)
	assert.Contains(t, text, "details.bound.url=[url]")
	assert.Contains(t, text, "details.db_password=[redacted]")
	assert.Contains(t, text, "details.payload=[unsupported value]")
	for _, forbidden := range []string{"fixture", "private.invalid", "path-secret", "query-secret", "fragment-secret", "oversized", "not visible"} {
		assert.NotContains(t, text, forbidden)
	}
	checkMode(t, dir, 0o700)
	for _, name := range []string{path, path + ".1", path + ".rotate.lock"} {
		checkMode(t, name, 0o600)
	}
}

func TestUnsafeLogPathAndClosedSink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".punt-labs", "sample", "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	target := filepath.Join(home, "unrelated")
	require.NoError(t, os.WriteFile(target, nil, 0o644))
	path := filepath.Join(dir, "sample.log")
	require.NoError(t, os.Symlink(target, path))
	_, err := New("sample", "daemon")
	require.Error(t, err)
	checkMode(t, target, 0o644)
	require.NoError(t, os.Remove(path))
	h, err := New("sample", "daemon", WithLevel(slog.LevelDebug))
	require.NoError(t, err)
	require.NoError(t, h.Close())
	assert.ErrorIs(t, h.Handle(context.Background(), slog.Record{Message: "closed", Level: slog.LevelDebug}), os.ErrClosed)
}

func checkMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode().Perm(), path)
}
