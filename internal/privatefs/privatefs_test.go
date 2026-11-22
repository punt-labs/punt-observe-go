package privatefs

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrentFileCreation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := Open("sample")
	require.NoError(t, err)
	defer func() { require.NoError(t, dir.Close()) }()
	for round := range 25 {
		name := "lock-" + strconv.Itoa(round)
		start := make(chan struct{})
		var group sync.WaitGroup
		var results [16]error
		for i := range results {
			group.Go(func() {
				<-start
				f, err := dir.FileWith(name, os.O_CREATE|os.O_RDWR)
				if err == nil {
					err = f.Close()
				}
				results[i] = err
			})
		}
		close(start)
		group.Wait()
		require.NoError(t, errors.Join(results[:]...), "round %d", round)
	}
}
