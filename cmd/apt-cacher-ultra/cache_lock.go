package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockDaemonCache prevents concurrent daemon startup/GC in one cache now that
// binding the proxy port happens after maintenance. CLI readers remain usable.
// Keep the file in place: unlinking a locked inode would allow a second owner.
func lockDaemonCache(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "daemon.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("daemon lock is not a regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock cache %q (another daemon may be using it): %w", dir, err)
	}
	return f, nil
}

// lockDaemonStorage keeps both the pool and its database exclusive to one
// daemon. Sharing either with a different cache can make startup GC delete
// files that the other daemon still needs. CLI readers do not take these locks.
func lockDaemonStorage(dir, dbDir string) (func(), error) {
	cacheLock, err := lockDaemonCache(dir)
	if err != nil {
		return nil, err
	}
	var dbLock *os.File
	unlock := func() {
		if dbLock != nil {
			_ = dbLock.Close()
		}
		_ = cacheLock.Close()
	}
	if dbDir == "" || dbDir == dir {
		return unlock, nil
	}
	cacheInfo, err := os.Stat(dir)
	if err != nil {
		unlock()
		return nil, err
	}
	dbInfo, err := os.Stat(dbDir)
	if err != nil {
		unlock()
		return nil, err
	}
	// Aliases (including symlinks) must not try to lock the same inode twice.
	if os.SameFile(cacheInfo, dbInfo) {
		return unlock, nil
	}
	dbLock, err = lockDaemonCache(dbDir)
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
