package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNewReturnsWhenRotationLockIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".punt-labs", "sample", "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	lock, err := os.OpenFile(filepath.Join(dir, "sample.log.rotate.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer func() { assert.NoError(t, lock.Close()) }()
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h, err := New("sample", "cli")
		if h != nil {
			err = errors.Join(err, h.Close())
		}
		result <- err
	}()
	defer func() {
		assert.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
		<-done
	}()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "rotation lock")
	case <-time.After(5 * time.Second):
		t.Fatal("logging.New did not return while another descriptor held the rotation lock")
	}
}

func TestIndependentWritersRotate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".punt-labs", "sample", "logs")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "sample.log")
	line := "Previously completed a safe operation.\n"
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(line, (maxFileBytes-100)/len(line))), 0o600))

	roles := []string{"cli", "server", "hook"}
	children := make([]*exec.Cmd, 0, len(roles))
	for _, role := range roles {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWriterProcess$")
		cmd.Env = append(os.Environ(), "PUNT_TEST_WRITER="+role)
		require.NoError(t, cmd.Start())
		children = append(children, cmd)
	}
	for _, cmd := range children {
		require.NoError(t, cmd.Wait())
	}
	var combined string
	for _, name := range []string{path + ".1", path} {
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(raw), maxFileBytes)
		assert.True(t, strings.HasSuffix(string(raw), "\n"))
		checkMode(t, name, 0o600)
		combined += string(raw)
	}
	for _, role := range roles {
		for i := range 40 {
			assert.Equal(t, 1, strings.Count(combined, fmt.Sprintf("id=%s-%03d\n", role, i)))
		}
	}
}

func TestRolloverWaitIsBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h, err := New("sample", "cli")
	require.NoError(t, err)
	defer func() { assert.NoError(t, h.Close()) }()
	path := filepath.Join(home, ".punt-labs", "sample", "logs", "sample.log")
	require.NoError(t, os.Truncate(path, maxFileBytes))
	lock, err := os.OpenFile(path+".rotate.lock", os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { assert.NoError(t, lock.Close()) }()
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "Completed operation", 0)
	start := time.Now()
	err = h.Handle(t.Context(), record)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.NoFileExists(t, path+".1")
	require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
	require.NoError(t, h.Handle(t.Context(), record))
	assert.FileExists(t, path+".1")
}

func TestWriterProcess(t *testing.T) {
	role := os.Getenv("PUNT_TEST_WRITER")
	if role == "" {
		t.Skip("subprocess only")
	}
	h, err := New("sample", role)
	require.NoError(t, err)
	log := slog.New(h)
	for i := range 40 {
		log.Info("Completed operation", "id", fmt.Sprintf("%s-%03d", role, i))
	}
	require.NoError(t, h.Close())
}

func TestBackupRetention(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h, err := New("sample", "cli")
	require.NoError(t, err)
	defer func() { assert.NoError(t, h.Close()) }()
	path := filepath.Join(home, ".punt-labs", "sample", "logs", "sample.log")
	for i := range backupCount + 2 {
		require.NoError(t, os.Truncate(path, maxFileBytes))
		require.NoError(t, h.sink.write(t.Context(), []byte("Completed: "+strconv.Itoa(i)+"\n")))
	}
	files, err := filepath.Glob(path + ".[0-9]*")
	require.NoError(t, err)
	require.Len(t, files, backupCount)
	for _, file := range files {
		info, err := os.Stat(file)
		require.NoError(t, err)
		assert.LessOrEqual(t, info.Size(), int64(maxFileBytes))
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}
