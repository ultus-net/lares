package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the exclusive interprocess lock that serializes a pass, a reboot,
// and a hold change.
//
// A second caller gets ErrLocked instead of waiting, so a timer pass never
// queues behind a stuck one. The lock is not reentrant: a caller that already
// holds it gets ErrLocked too. One code path takes the lock and calls the
// unlocked helper, rather than nesting.
func (s *Store) Lock() error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.locked {
		return ErrLocked
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return fmt.Errorf("lock state directory: %w", err)
	}
	s.lockFile = f
	s.locked = true
	return nil
}

// Unlock releases the lock. Calling it when the lock is not held is a no-op.
func (s *Store) Unlock() error {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if !s.locked {
		return nil
	}
	f := s.lockFile
	s.lockFile = nil
	s.locked = false
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		f.Close()
		return fmt.Errorf("unlock state directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state lock: %w", err)
	}
	return nil
}
