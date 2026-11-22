// Package privatefs opens private, owner-controlled state beneath the home directory.
package privatefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var toolName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// ValidTool reports whether a name can be used as a tool directory and signal prefix.
func ValidTool(tool string) bool {
	return len(tool) <= 64 && toolName.MatchString(tool)
}

// Dir pins a private directory for operations on directory-relative filenames.
type Dir struct {
	*os.File
	Path string
}

// Open resolves and prepares the requested components under ~/.punt-labs.
func Open(components ...string) (*Dir, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("cannot resolve an absolute home directory")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, name := range append([]string{".punt-labs"}, components...) {
		next, childErr := openDir(fd, name)
		_ = unix.Close(fd)
		if childErr != nil {
			return nil, fmt.Errorf("prepare directory %s: %w", name, childErr)
		}
		fd = next
		home = filepath.Join(home, name)
	}
	return &Dir{File: os.NewFile(uintptr(fd), home), Path: home}, nil
}

func openDir(parent int, name string) (int, error) {
	if !validLeaf(name) {
		return -1, unix.EINVAL
	}
	if err := unix.Mkdirat(parent, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := secure(fd, unix.S_IFDIR, 0o700); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// FileWith opens a private regular file without following links or opening a FIFO in blocking mode.
func (d *Dir) FileWith(name string, flags int) (*os.File, error) {
	if !validLeaf(name) {
		return nil, unix.EINVAL
	}
	parent := int(d.Fd())
	flags |= unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, name, flags, 0o600)
	if errors.Is(err, unix.ENOENT) && flags&(unix.O_CREAT|unix.O_EXCL) == unix.O_CREAT {
		// Darwin openat can report ENOENT to the loser of a concurrent create (golang/go#81246).
		fd, err = unix.Openat(parent, name, flags&^unix.O_CREAT, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := secure(fd, unix.S_IFREG, 0o600); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Tighten repairs existing private files and ignores absent ones.
func (d *Dir) Tighten(names ...string) error {
	for _, name := range names {
		if !validLeaf(name) {
			return unix.EINVAL
		}
		var stat unix.Stat_t
		fd := int(d.Fd())
		err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := checkOwner(stat, unix.S_IFREG); err != nil {
			return err
		}
		if uint32(stat.Mode)&0o7777 != 0o600 {
			if err := unix.Fchmodat(fd, name, 0o600, unix.AT_SYMLINK_NOFOLLOW); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("secure %s: %w", name, err)
			}
		}
	}
	return nil
}

// Rename replaces a leaf using directory-relative operations.
func (d *Dir) Rename(old, next string) error {
	if !validLeaf(old) || !validLeaf(next) {
		return unix.EINVAL
	}
	return unix.Renameat(int(d.Fd()), old, int(d.Fd()), next)
}

// Remove removes a leaf without resolving the path again.
func (d *Dir) Remove(name string) error {
	if !validLeaf(name) {
		return unix.EINVAL
	}
	return unix.Unlinkat(int(d.Fd()), name, 0)
}

// Lock obtains an exclusive lock, retrying signal interruptions.
func Lock(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// LockContext waits for a lock only while ctx permits.
func LockContext(ctx context.Context, file *os.File) error {
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for ctx.Err() == nil {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// Unlock releases an exclusive lock while leaving the file open.
func Unlock(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func validLeaf(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func secure(fd int, kind, mode uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if err := checkOwner(stat, kind); err != nil {
		return err
	}
	if uint32(stat.Mode)&0o7777 != mode {
		return unix.Fchmod(fd, mode)
	}
	return nil
}

func checkOwner(stat unix.Stat_t, kind uint32) error {
	if uint32(stat.Mode)&unix.S_IFMT != kind || int(stat.Uid) != os.Geteuid() ||
		(kind == unix.S_IFREG && stat.Nlink != 1) {
		return errors.New("unexpected file type, links or owner")
	}
	return nil
}
