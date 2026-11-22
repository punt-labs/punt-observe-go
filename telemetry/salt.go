package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
)

const saltName = "telemetry.salt"
const saltBytes = 32

func installationSalt(ctx context.Context) (salt []byte, err error) {
	dir, err := privatefs.Open()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	lock, err := dir.FileWith(".telemetry-salt.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := privatefs.LockContext(ctx, lock); err != nil {
		return nil, err
	}
	file, err := dir.FileWith(saltName, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return createSalt(dir)
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	salt, err = io.ReadAll(io.LimitReader(file, saltBytes+1))
	if err != nil {
		return nil, err
	}
	if len(salt) != saltBytes {
		return nil, errors.New("invalid installation salt")
	}
	return salt, nil
}

func createSalt(dir *privatefs.Dir) ([]byte, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	tmp := "." + saltName + "." + hex.EncodeToString(nonce)
	file, err := dir.FileWith(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Remove(tmp) }()
	_, writeErr := file.Write(salt)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return nil, err
	}
	if err := dir.Rename(tmp, saltName); err != nil {
		return nil, err
	}
	return salt, dir.Sync()
}
