//go:build linux || darwin || freebsd

package session

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lockProfile(path string) (string, *os.File, error) {
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("save_dir must be absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", nil, err
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	fd, err := unix.Open(filepath.Join(path, ".blastem-mcp.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", nil, err
	}
	file := os.NewFile(uintptr(fd), "save profile lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return "", nil, fmt.Errorf("save profile is already in use: %w", err)
	}
	return path, file, nil
}
