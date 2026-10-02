package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ObserveTerminalPurgeTarget captures only descriptor-based filesystem identity;
// it never starts Herdr or mutates a directory. Missing roots/sessions are pins
// of absence: a later directory appearing there is a conflict, not permission.
func ObserveTerminalPurgeTarget(ctx context.Context, root, name string) (LifecyclePurgeTarget, error) {
	target := LifecyclePurgeTarget{Root: root}
	if ctx == nil {
		return target, errors.New("terminal purge context is nil")
	}
	if err := ctx.Err(); err != nil {
		return target, err
	}
	if err := validatePurgeTarget(target); err != nil {
		return target, err
	}
	if !validSessionName(name) || name == "default" {
		return target, errors.New("unsafe Herdr purge session name")
	}
	rootFD, err := openPurgeRootPath(root)
	if errors.Is(err, unix.ENOENT) {
		return target, nil
	}
	if err != nil {
		return target, err
	}
	defer unix.Close(rootFD)
	target.RootDevice, target.RootInode, target.RootBirthNS, err = purgeDirectoryIdentity(rootFD)
	if err != nil {
		return target, err
	}
	sessionsFD, err := openPurgeDirectoryAt(rootFD, "sessions")
	if errors.Is(err, unix.ENOENT) {
		return target, unix.Fsync(rootFD)
	}
	if err != nil {
		return target, err
	}
	defer unix.Close(sessionsFD)
	sessionFD, err := openPurgeDirectoryAt(sessionsFD, name)
	if errors.Is(err, unix.ENOENT) {
		return target, unix.Fsync(sessionsFD)
	}
	if err != nil {
		return target, err
	}
	defer unix.Close(sessionFD)
	target.SessionDevice, target.SessionInode, target.SessionBirthNS, err = purgeDirectoryIdentity(sessionFD)
	if err != nil {
		return target, err
	}
	target.SessionExists = true
	return target, nil
}

func verifyTerminalPurgeTarget(ctx context.Context, target LifecyclePurgeTarget, name string) (bool, error) {
	if err := validatePurgeTarget(target); err != nil {
		return false, err
	}
	current, err := ObserveTerminalPurgeTarget(ctx, target.Root, name)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EXDEV) {
		return false, lifecycleConflictReason("directory_identity_changed", "Herdr purge path became a symlink, non-directory, or nested mount")
	}
	if err != nil {
		return false, err
	}
	if current.RootInode == 0 {
		return true, nil
	}
	if current.RootDevice != target.RootDevice || current.RootInode != target.RootInode || target.RootBirthNS != 0 && current.RootBirthNS != target.RootBirthNS {
		return false, lifecycleConflictReason("root_identity_changed", "Herdr root directory differs from the durable purge pin")
	}
	if !current.SessionExists {
		return true, nil
	}
	if !target.SessionExists || current.SessionDevice != target.SessionDevice || current.SessionInode != target.SessionInode || target.SessionBirthNS != 0 && current.SessionBirthNS != target.SessionBirthNS {
		return false, lifecycleConflictReason("session_identity_changed", "Herdr session directory differs from the durable purge pin")
	}
	return false, nil
}

// Resolve every ancestor through an FD, checking ownership and path safety on
// that very object. This avoids separate pathname validation/open races. Sticky
// shared ancestors such as /tmp are permitted, as in the existing Herdr policy.
func openPurgeRootPath(path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	for index, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("open Herdr purge path component: %w", err)
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		if stat.Uid != 0 && stat.Uid != uint32(os.Getuid()) || stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
			_ = unix.Close(fd)
			return -1, errors.New("Herdr purge ancestor is not a trusted directory")
		}
		if index == len(parts)-1 && (stat.Uid != uint32(os.Getuid()) || stat.Mode&0o077 != 0) {
			_ = unix.Close(fd)
			return -1, errors.New("Herdr purge root must be owner-only")
		}
	}
	return fd, nil
}

func openPurgeDirectoryAt(parent int, name string) (int, error) {
	fd, err := unix.Openat2(parent, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	// Native Herdr creates 0755 children under its private 0700 root. Read
	// and search bits on these descendants do not grant access through that
	// root; foreign ownership or group/other writes remain unsafe.
	if stat.Uid != uint32(os.Getuid()) || stat.Mode&0o022 != 0 {
		_ = unix.Close(fd)
		return -1, errors.New("Herdr purge directory must be owned by the current user and not group- or other-writable")
	}
	return fd, nil
}

func purgeDirectoryIdentity(fd int) (device, inode uint64, birthNS int64, err error) {
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return
	}
	device, inode = uint64(stat.Dev), stat.Ino
	var extended unix.Statx_t
	err = unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &extended)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		err = nil // Older kernel/filesystem: preserve weaker dev/inode evidence.
		return
	}
	if err != nil {
		return
	}
	if extended.Mask&unix.STATX_BTIME != 0 {
		birthNS = extended.Btime.Sec*1_000_000_000 + int64(extended.Btime.Nsec)
	}
	return
}
