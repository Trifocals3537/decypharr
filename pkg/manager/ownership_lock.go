package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sirrobot01/decypharr/internal/filelock"
	"github.com/sirrobot01/decypharr/internal/safepath"
)

// ownershipRoot keeps the validated directory capability and advisory lock
// alive together so mutations cannot be redirected by reopening the root path.
type ownershipRoot struct {
	absolute string
	root     *os.Root
	lock     *filelock.Lock
}

func acquireOwnershipRoot(path, lockName string, create bool, timeout, retryDelay time.Duration) (*ownershipRoot, bool, error) {
	var (
		absolute string
		rooted   *os.Root
		err      error
	)
	if create {
		rooted, absolute, err = safepath.EnsureOpenRoot(path, 0o755)
	} else {
		absolute, err = safepath.ValidateRoot(path)
	}
	if err != nil {
		return nil, false, err
	}

	if rooted == nil {
		rooted, _, err = safepath.OpenRoot(absolute)
		if err != nil {
			if !create && errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("open ownership root: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	locked, lockErr := filelock.AcquireContext(ctx, rooted, lockName, 0o600, retryDelay)
	cancel()
	if lockErr != nil {
		_ = rooted.Close()
		return nil, false, fmt.Errorf("lock ownership root: %w", lockErr)
	}
	return &ownershipRoot{absolute: absolute, root: rooted, lock: locked}, true, nil
}

func (ownership *ownershipRoot) close() error {
	if ownership == nil {
		return nil
	}
	var verifyErr, lockErr, rootErr error
	if ownership.root != nil {
		verifyErr = safepath.VerifyOpenRoot(ownership.root, ownership.absolute)
	}
	if ownership.lock != nil {
		lockErr = ownership.lock.Unlock()
		ownership.lock = nil
	}
	if ownership.root != nil {
		rootErr = ownership.root.Close()
		ownership.root = nil
	}
	return errors.Join(verifyErr, lockErr, rootErr)
}
