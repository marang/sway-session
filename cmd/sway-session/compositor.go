package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// compositorIdentity derives a non-secret lifetime identity from the Sway IPC
// socket inode. Sway config reloads preserve the socket; a compositor restart
// replaces it, even when the pathname is reused.
func compositorIdentity(socket string) (string, error) {
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return "", errors.New("sway IPC socket must be a clean absolute path")
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return "", fmt.Errorf("inspect sway IPC socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("sway IPC endpoint is not a Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("sway IPC socket metadata is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("sway IPC socket is not owned by the current user")
	}
	evidence := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%d", socket, stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, stat.Uid)
	digest := sha256.Sum256([]byte(evidence))
	return hex.EncodeToString(digest[:]), nil
}
