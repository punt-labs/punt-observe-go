package telemetry

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupAndIdlePruningReclaimsSpace(t *testing.T) {
	home := freshHome(t)
	s, err := openSQLite(t.Context(), "sample", 1)
	require.NoError(t, err)
	addExpired(t, s)
	var before int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&before))
	require.NoError(t, s.close(t.Context()))

	c := newClient("sample", Config{RetentionDays: 1}, 50*time.Millisecond, testSchemas)
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	waitReady(t, c)
	db := connectTest(t, sqlitePath(home))
	var count, after int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM signals").Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&after))
	assert.Less(t, after, before)
	_, err = db.ExecContext(t.Context(), insertSignal, "event", "sample.cli.finish", 1, 1, 1, "", "", "", 0, 0, "{}", "")
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		var n int
		return db.QueryRowContext(t.Context(), "SELECT count(*) FROM signals").Scan(&n) == nil && n == 0
	}, 2*time.Second, 25*time.Millisecond)
}

func TestPruneReclaimsMultiplePages(t *testing.T) {
	freshHome(t)
	s, err := openSQLite(t.Context(), "sample", 1)
	require.NoError(t, err)
	defer func() { assert.NoError(t, s.close(t.Context())) }()
	addExpired(t, s)
	var before, after int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&before))
	require.NoError(t, s.prune(t.Context()))
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&after))
	assert.Greater(t, before-after, 1, "one vacuum pass should reclaim multiple free pages")
}

func addExpired(t *testing.T, s *sqliteExporter) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `
WITH RECURSIVE state(n) AS (
 SELECT 1 UNION ALL SELECT n+1 FROM state WHERE n<80
)
INSERT INTO signals
SELECT 'event','sample.cli.finish',1,1,1,'','','',0,0,hex(zeroblob(4000)),'' FROM state
`)
	require.NoError(t, err)
}

func TestPrivateJournalAndExistingSidecars(t *testing.T) {
	home := freshHome(t)
	path := sqlitePath(home)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		require.NoError(t, os.WriteFile(path+suffix, nil, 0o644))
	}
	dir, err := privatefs.Open("sample")
	require.NoError(t, err)
	require.NoError(t, prepareDatabase(dir))
	require.NoError(t, dir.Close())
	assertMode(t, filepath.Dir(path), 0o700)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		assertMode(t, path+suffix, 0o600)
		if suffix != "" {
			require.NoError(t, os.Remove(path+suffix))
		}
	}
	s, err := openSQLite(t.Context(), "sample", 90)
	require.NoError(t, err)
	defer func() { assert.NoError(t, s.close(t.Context())) }()
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), insertSignal, "event", "sample.cli.finish", 1, 1, 1, "", "", "", 0, 0, "{}", "")
	require.NoError(t, err)
	assertMode(t, path+"-journal", 0o600)
	require.NoError(t, tx.Rollback())
}

func TestConcurrentLocalStores(t *testing.T) {
	home := freshHome(t)
	var group sync.WaitGroup
	for range 5 {
		group.Go(func() {
			c := New("sample", Config{}, testSchemas...)
			<-c.Ready()
			recordExample(c)
			c.Shutdown(t.Context())
		})
	}
	group.Wait()
	var count int
	db := connectTest(t, sqlitePath(home))
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM signals").Scan(&count))
	assert.Equal(t, 20, count)
}

func TestSQLiteWritesRespectProcessLockAndDeadline(t *testing.T) {
	freshHome(t)
	s, err := openSQLite(t.Context(), "sample", 1)
	require.NoError(t, err)
	defer func() { assert.NoError(t, s.close(t.Context())) }()
	dir, err := privatefs.Open("sample")
	require.NoError(t, err)
	defer func() { assert.NoError(t, dir.Close()) }()
	lock, err := dir.FileWith(".telemetry.lock", os.O_RDWR)
	require.NoError(t, err)
	defer func() { assert.NoError(t, lock.Close()) }()
	setup, closeSetup := context.WithTimeout(t.Context(), time.Second)
	defer closeSetup()
	require.NoError(t, privatefs.LockContext(setup, lock))
	action := func(*sql.Tx) error { return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.write(ctx, action), context.DeadlineExceeded)
	require.NoError(t, privatefs.Unlock(lock))
	require.NoError(t, s.write(t.Context(), action))
}
