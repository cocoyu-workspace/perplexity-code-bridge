//go:build !windows

package proxy

import (
	"os"
	"path/filepath"
	"syscall"
)

func withStateFileLock(targetPath string, fn func() error) error {
	if targetPath == "" {
		return fn()
	}
	lockPath := targetPath + ".lock"
	if dir := filepath.Dir(lockPath); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fn()
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}
