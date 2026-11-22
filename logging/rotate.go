package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
)

const (
	maxFileBytes = 5_242_880
	backupCount  = 5
)

type sink struct {
	mu   sync.Mutex
	dir  *privatefs.Dir
	name string
}

func (s *sink) locked(ctx context.Context, action func() error) (err error) {
	if err := s.lockLocal(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if s.dir == nil {
		return os.ErrClosed
	}
	lock, err := s.dir.FileWith(s.name+".rotate.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if ctx.Done() == nil {
		err = privatefs.Lock(lock)
	} else {
		err = privatefs.LockContext(ctx, lock)
	}
	if err != nil {
		return err
	}
	return action()
}

func (s *sink) lockLocal(ctx context.Context) error {
	if ctx.Done() == nil {
		s.mu.Lock()
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if s.mu.TryLock() {
				return nil
			}
		}
	}
}

func (s *sink) setup() error {
	return s.locked(context.Background(), func() error {
		if err := s.secureBackups(); err != nil {
			return err
		}
		active, err := s.openActive()
		if err != nil {
			return err
		}
		info, err := active.Stat()
		if err := errors.Join(err, active.Close()); err != nil {
			return err
		}
		if info.Size() <= maxFileBytes {
			return nil
		}
		if err := s.rotate(true); err != nil {
			return err
		}
		active, err = s.openActive()
		if err != nil {
			return err
		}
		return active.Close()
	})
}

func (s *sink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return nil
	}
	err := s.dir.Close()
	s.dir = nil
	return err
}

func (s *sink) write(ctx context.Context, line []byte) error {
	return s.locked(ctx, func() error {
		active, err := s.openActive()
		if err != nil {
			return err
		}
		info, err := active.Stat()
		if err != nil {
			return errors.Join(err, active.Close())
		}
		offset := info.Size()
		if offset+int64(len(line)) > maxFileBytes {
			if err := active.Close(); err != nil {
				return err
			}
			if err := s.rotate(info.Size() > maxFileBytes); err != nil {
				return err
			}
			active, err = s.openActive()
			if err != nil {
				return err
			}
			offset = 0
		}
		n, err := active.Write(line)
		if n != len(line) {
			err = errors.Join(err, io.ErrShortWrite, active.Truncate(offset))
		}
		return errors.Join(err, active.Close())
	})
}

func (s *sink) openActive() (*os.File, error) {
	return s.dir.FileWith(s.name, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
}

func (s *sink) backup(n int) string {
	return fmt.Sprintf("%s.%d", s.name, n)
}

func (s *sink) secureBackups() error {
	for n := 1; n <= backupCount; n++ {
		name := s.backup(n)
		f, err := s.dir.FileWith(name, os.O_RDONLY)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, statErr := f.Stat()
		if err := errors.Join(statErr, f.Close()); err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			if err := s.dir.Remove(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sink) rotate(oversize bool) error {
	if err := s.secureBackups(); err != nil {
		return err
	}
	if oversize {
		return s.dir.Remove(s.name)
	}
	for n := backupCount; n > 1; n-- {
		err := s.dir.Rename(s.backup(n-1), s.backup(n))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.dir.Rename(s.name, s.backup(1))
}
