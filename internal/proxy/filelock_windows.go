//go:build windows

package proxy

import "sync"

var windowsFallbackFileLock sync.Mutex

func withStateFileLock(targetPath string, fn func() error) error {
	windowsFallbackFileLock.Lock()
	defer windowsFallbackFileLock.Unlock()
	return fn()
}
