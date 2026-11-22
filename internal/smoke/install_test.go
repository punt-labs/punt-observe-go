package smoke

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

func TestInstalledSmoke(t *testing.T) {
	bin := t.TempDir()
	home := t.TempDir()
	install := exec.CommandContext(t.Context(), "go", "install", "./cmd/punt-observe-smoke")
	install.Dir = "../.."
	install.Env = append(os.Environ(), "GOBIN="+bin, "CGO_ENABLED=0")
	out, err := install.CombinedOutput()
	require.NoError(t, err, "%s", out)
	imports := exec.CommandContext(t.Context(), "go", "list", "-f", "{{join .Imports \"\\n\"}}", "./internal/smoke")
	imports.Dir = "../.."
	out, err = imports.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.NotContains(t, string(out), "go.opentelemetry.io")
	assert.NotContains(t, string(out), "database/sql")
	binary := filepath.Join(bin, "punt-observe-smoke")
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Dir = bin
		cmd.Env = append(os.Environ(), "HOME="+home, "PUNT_TELEMETRY=", "PUNT_OBSERVE_SMOKE_TELEMETRY=")
		return cmd
	}
	roles := []string{"cli", "server", "hook"}
	var children []*exec.Cmd
	for _, role := range roles {
		cmd := command("-role", role, "-records", "48")
		require.NoError(t, cmd.Start())
		children = append(children, cmd)
	}
	for _, cmd := range children {
		require.NoError(t, cmd.Wait())
	}
	require.NoError(t, command("-spawn").Run())
	require.Error(t, command("-spawn", "-fail").Run())

	root := filepath.Join(home, ".punt-labs", tool)
	logDir := filepath.Join(root, "logs")
	privateMode(t, root, 0o700)
	privateMode(t, logDir, 0o700)
	privateMode(t, filepath.Join(home, ".punt-labs", "telemetry.salt"), 0o600)
	base := filepath.Join(logDir, tool+".log")
	files, err := filepath.Glob(base + "*")
	require.NoError(t, err)
	require.FileExists(t, base+".1")
	require.LessOrEqual(t, len(files), 7)
	forbidden := []string{"fixture-user", "fixture-pass", "example.invalid", "path-secret", "query-secret", "fragment-secret", "fixture-auth", "fixture-person"}
	verifyLogs(t, files, forbidden)
	store := filepath.Join(root, "telemetry.db")
	verifyStore(t, store, forbidden, 5)
	t.Logf("installed example: %d log files (including lock), rotated; 144 load records, two detached outcomes, five telemetry spans", len(files))

	before, err := os.Stat(base)
	require.NoError(t, err)
	holdRotationLock(t, base+".rotate.lock")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-role", "cli")
	cmd.Dir = bin
	cmd.Env = command().Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err = cmd.Output()
	elapsed := time.Since(start)
	require.NoError(t, err, "startup during lock contention: %s", &stderr)
	assert.Empty(t, out)
	assert.Equal(t, "Private diagnostics unavailable: rotation lock timed out; continuing without file logging\n", stderr.String())
	after, err := os.Stat(base)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size(), "contended startup must not write to the log")
	verifyStore(t, store, forbidden, 6)
	t.Logf("separate process held rotation lock: example completed in %s, no file logging, one additional span, event and counter", elapsed.Round(time.Millisecond))
}

func holdRotationLock(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHoldRotationLockProcess$")
	cmd.Env = append(os.Environ(), "PUNT_SMOKE_ROTATION_LOCK="+path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		assert.NoError(t, in.Close())
		assert.NoError(t, cmd.Wait())
	})
	ready, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", ready)
}

func TestHoldRotationLockProcess(t *testing.T) {
	path := os.Getenv("PUNT_SMOKE_ROTATION_LOCK")
	if path == "" {
		t.Skip("subprocess only")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { assert.NoError(t, file.Close()) }()
	require.NoError(t, unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	_, err = fmt.Fprintln(os.Stdout, "locked")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
}

func verifyLogs(t *testing.T, files, forbidden []string) {
	t.Helper()
	var combined strings.Builder
	prefix := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \[(INFO|DEBUG|WARNING)\] punt-observe-smoke\.`)
	for _, path := range files {
		privateMode(t, path, 0o600)
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(b), 5_242_880)
		if len(b) == 0 {
			continue
		}
		assert.Equal(t, byte('\n'), b[len(b)-1])
		for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
			assert.True(t, prefix.MatchString(line), "invalid record prefix in %s", filepath.Base(path))
			assert.NotContains(t, line, "\x1b")
		}
		combined.Write(b)
	}
	text := combined.String()
	for _, secret := range forbidden {
		assert.False(t, strings.Contains(text, secret), "fixture survived redaction")
	}
	assert.Equal(t, 144, strings.Count(text, "Diagnostic load fixture"))
	for _, message := range []string{"Spawning detached smoke worker", "Detached smoke worker completed", "Detached smoke worker failed", "worker: Started", "worker: Completed", "worker: Diagnostic smoke operation failed"} {
		assert.True(t, strings.Contains(text, message), "missing diagnostic %q", message)
	}
	spawns := regexp.MustCompile(`Spawning detached smoke worker request_id=(smoke-\d+)`).FindAllStringSubmatch(text, -1)
	require.Len(t, spawns, 2)
	assert.NotEqual(t, spawns[0][1], spawns[1][1])
	for _, spawn := range spawns {
		assert.Equal(t, 1, strings.Count(text, "worker: Started diagnostic smoke operation request_id="+spawn[1]+"\n"))
	}
}

func verifyStore(t *testing.T, path string, forbidden []string, expected int) {
	t.Helper()
	privateMode(t, path, 0o600)
	db, err := sql.Open("sqlite", path+"?mode=ro")
	require.NoError(t, err)
	defer func() { assert.NoError(t, db.Close()) }()
	var integrity string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&integrity))
	assert.Equal(t, "ok", integrity)
	for _, signal := range []string{"span", "event", "counter"} {
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM signals WHERE signal=?", signal).Scan(&count))
		assert.Equal(t, expected, count, signal)
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, secret := range forbidden {
		assert.False(t, strings.Contains(string(data), secret), "fixture reached telemetry")
	}
	paths, err := filepath.Glob(path + "-*")
	require.NoError(t, err)
	for _, sidecar := range paths {
		privateMode(t, sidecar, 0o600)
	}
}

func privateMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode().Perm(), fmt.Sprintf("mode of %s", path))
}
