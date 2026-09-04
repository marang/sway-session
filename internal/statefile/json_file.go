// Package statefile provides private, bounded, atomic JSON state files.
package statefile

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	DirectoryMode   = 0o700
	RegularFileMode = 0o600
	MaxFileSize     = 16 * 1024 * 1024

	temporaryNameAttempts = 128
	safeResolveFlags      = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS
	lockRetryDelay        = 10 * time.Millisecond
)

// JSONFile is one validated state document in a private state directory.
type JSONFile[T any] struct {
	directory       string
	name            string
	validate        func(*T) error
	syncAfterRename func(*os.File) error
}

// LockedPrivateDirectory exposes descriptor-relative operations for one
// verified owner-only directory while WithPrivateDirectoryLock holds its
// exclusive advisory lock. The descriptor, rather than the mutable pathname,
// anchors every operation in a compound transaction.
type LockedPrivateDirectory struct {
	file *os.File
}

// CommitOutcomeUnknownError reports a replacement that is already visible in
// the current filesystem namespace but whose durability could not be
// confirmed. Callers must reload and reconcile before retrying the mutation or
// performing dependent external side effects.
type CommitOutcomeUnknownError struct {
	Cause error
}

func (err *CommitOutcomeUnknownError) Error() string {
	return fmt.Sprintf("state commit is visible but durability is unknown: %v", err.Cause)
}

func (err *CommitOutcomeUnknownError) Unwrap() error {
	return err.Cause
}

// NewJSONFile describes a state file. Paths and permissions are checked when
// LoadInto, Save, or Update accesses it.
func NewJSONFile[T any](directory string, name string, validate func(*T) error) JSONFile[T] {
	return JSONFile[T]{
		directory:       directory,
		name:            name,
		validate:        validate,
		syncAfterRename: syncFile,
	}
}

// OpenPrivateDirectory opens a symlink-free owner-only directory and can
// create missing components. The returned descriptor remains attached to the
// verified directory if a pathname component is replaced concurrently.
func OpenPrivateDirectory(path string, create bool) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("private directory must be a clean absolute path")
	}
	return openStateDirectory(path, create)
}

// CreatePrivateFileAt creates one owner-only regular file relative to an
// already opened private directory descriptor. It is intended for callers
// that must keep a pathname pinned across validation and creation.
func CreatePrivateFileAt(directory *os.File, name string, data []byte) error {
	if directory == nil {
		return errors.New("private directory descriptor is nil")
	}
	if err := validatePrivateFileName(name); err != nil {
		return err
	}
	if len(data) > MaxFileSize {
		return fmt.Errorf("private file is too large: %d bytes exceeds %d", len(data), MaxFileSize)
	}
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect private directory descriptor: %w", err)
	}
	if !info.IsDir() {
		return errors.New("private directory descriptor is not a directory")
	}
	if err := checkOwner(info); err != nil {
		return fmt.Errorf("private directory descriptor: %w", err)
	}
	if err := checkMode(info, DirectoryMode); err != nil {
		return fmt.Errorf("private directory descriptor: %w", err)
	}
	return createPrivateFileAt(directory, name, data)
}

// WithPrivateDirectoryLock runs action while holding an exclusive lock on a
// symlink-free owner-only directory. All compound file operations must use the
// supplied descriptor-relative directory so pathname replacement cannot move
// later steps onto a different inode and lock domain.
func WithPrivateDirectoryLock(path string, action func(*LockedPrivateDirectory) error) error {
	return WithPrivateDirectoryLockContext(context.Background(), path, action)
}

// WithPrivateDirectoryLockContext is WithPrivateDirectoryLock with cancelable
// lock acquisition.
func WithPrivateDirectoryLockContext(ctx context.Context, path string, action func(*LockedPrivateDirectory) error) error {
	if ctx == nil {
		return errors.New("private directory lock context is nil")
	}
	if action == nil {
		return errors.New("private directory action is nil")
	}
	directory, err := OpenPrivateDirectory(path, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return err
	}
	defer unlockDirectory(directory)
	return action(&LockedPrivateDirectory{file: directory})
}

// Create creates one bounded owner-only regular file exactly once.
func (directory *LockedPrivateDirectory) Create(name string, data []byte) error {
	if err := directory.validateName(name); err != nil {
		return err
	}
	if len(data) > MaxFileSize {
		return fmt.Errorf("private file is too large: %d bytes exceeds %d", len(data), MaxFileSize)
	}
	return createPrivateFileAt(directory.file, name, data)
}

// Read reads one bounded owner-only regular file without following symlinks.
func (directory *LockedPrivateDirectory) Read(name string) ([]byte, error) {
	if err := directory.validateName(name); err != nil {
		return nil, err
	}
	return readPrivateRegularFileAt(directory.file, name)
}

// List returns at most max directory-entry names from the pinned directory.
func (directory *LockedPrivateDirectory) List(max int) ([]string, error) {
	if directory == nil || directory.file == nil {
		return nil, errors.New("locked private directory is nil")
	}
	return listPrivateFilesAt(directory.file, max)
}

// Remove removes one verified regular file. A missing file succeeds.
func (directory *LockedPrivateDirectory) Remove(name string) error {
	if err := directory.validateName(name); err != nil {
		return err
	}
	return removePrivateFileAt(directory.file, name)
}

func (directory *LockedPrivateDirectory) validateName(name string) error {
	if directory == nil || directory.file == nil {
		return errors.New("locked private directory is nil")
	}
	return validatePrivateFileName(name)
}

// CreatePrivateFile creates one bounded owner-only file exactly once. It is
// intended for random operation tokens whose names must never be replaced.
func CreatePrivateFile(directoryPath string, name string, data []byte) error {
	return CreatePrivateFileContext(context.Background(), directoryPath, name, data)
}

// CreatePrivateFileContext is CreatePrivateFile with cancelable lock
// acquisition.
func CreatePrivateFileContext(ctx context.Context, directoryPath string, name string, data []byte) error {
	if ctx == nil {
		return errors.New("private file create context is nil")
	}
	if err := validatePrivateFilePath(directoryPath, name); err != nil {
		return err
	}
	if len(data) > MaxFileSize {
		return fmt.Errorf("private file is too large: %d bytes exceeds %d", len(data), MaxFileSize)
	}
	directory, err := openStateDirectory(directoryPath, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return err
	}
	defer unlockDirectory(directory)
	return createPrivateFileAt(directory, name, data)
}

func createPrivateFileAt(directory *os.File, name string, data []byte) error {
	return createPrivateFileAtWithDirectorySync(directory, name, data, func() error { return directory.Sync() })
}

func createPrivateFileAtWithDirectorySync(directory *os.File, name string, data []byte, syncDirectory func() error) error {
	fd, err := openAt2(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint64(RegularFileMode))
	if err != nil {
		return fmt.Errorf("create private file: %w", err)
	}
	var created unix.Stat_t
	if err := unix.Fstat(fd, &created); err != nil {
		_ = unix.Close(fd)
		return &CommitOutcomeUnknownError{Cause: fmt.Errorf("inspect created private file: %w", err)}
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		_ = removeMatchingPrivateFileAt(directory, name, created)
		return errors.New("create private file: invalid file descriptor")
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = removeMatchingPrivateFileAt(directory, name, created)
		}
	}()
	if err := file.Chmod(RegularFileMode); err != nil {
		return err
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write private file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync private file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file: %w", err)
	}
	if syncDirectory == nil {
		syncDirectory = func() error { return directory.Sync() }
	}
	if err := syncDirectory(); err != nil {
		keep = true
		return &CommitOutcomeUnknownError{Cause: fmt.Errorf("sync private directory: %w", err)}
	}
	keep = true
	return nil
}

func removeMatchingPrivateFileAt(directory *os.File, name string, created unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if current.Dev != created.Dev || current.Ino != created.Ino {
		return nil
	}
	return unix.Unlinkat(int(directory.Fd()), name, 0)
}

// ReadPrivateFile reads one bounded owner-only regular file without following
// symlinks.
func ReadPrivateFile(directoryPath string, name string) ([]byte, error) {
	return ReadPrivateFileContext(context.Background(), directoryPath, name)
}

// ReadPrivateFileContext is ReadPrivateFile with cancelable lock acquisition.
func ReadPrivateFileContext(ctx context.Context, directoryPath string, name string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("private file read context is nil")
	}
	if err := validatePrivateFilePath(directoryPath, name); err != nil {
		return nil, err
	}
	directory, err := openStateDirectory(directoryPath, false)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_SH); err != nil {
		return nil, err
	}
	defer unlockDirectory(directory)
	return readPrivateRegularFileAt(directory, name)
}

// ListPrivateFiles returns at most max verified directory-entry names. It does
// not follow or open entries and fails when the directory contains more than
// the caller's explicit bound.
func ListPrivateFiles(directoryPath string, max int) ([]string, error) {
	return ListPrivateFilesContext(context.Background(), directoryPath, max)
}

// ListPrivateFilesContext is ListPrivateFiles with cancelable lock acquisition.
func ListPrivateFilesContext(ctx context.Context, directoryPath string, max int) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("private file list context is nil")
	}
	if max <= 0 {
		return nil, errors.New("private file list bound must be positive")
	}
	if err := validatePrivateFilePath(directoryPath, "placeholder"); err != nil {
		return nil, err
	}
	directory, err := openStateDirectory(directoryPath, false)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_SH); err != nil {
		return nil, err
	}
	defer unlockDirectory(directory)
	return listPrivateFilesAt(directory, max)
}

func listPrivateFilesAt(directory *os.File, max int) ([]string, error) {
	if max <= 0 {
		return nil, errors.New("private file list bound must be positive")
	}
	if _, err := directory.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind private directory: %w", err)
	}
	names := make([]string, 0)
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			names = append(names, entry.Name())
			if len(names) > max {
				return nil, fmt.Errorf("private directory contains more than %d entries", max)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("list private directory: %w", readErr)
		}
		if len(entries) == 0 {
			return nil, errors.New("list private directory returned no entries without completion")
		}
	}
	return names, nil
}

// ConsumePrivateFile atomically reads and removes a private file under the
// directory lock. A second consumer observes a missing file.
func ConsumePrivateFile(directoryPath string, name string) ([]byte, error) {
	return ConsumePrivateFileContext(context.Background(), directoryPath, name)
}

// ConsumePrivateFileContext is ConsumePrivateFile with cancelable lock
// acquisition.
func ConsumePrivateFileContext(ctx context.Context, directoryPath string, name string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("private file consume context is nil")
	}
	if err := validatePrivateFilePath(directoryPath, name); err != nil {
		return nil, err
	}
	directory, err := openStateDirectory(directoryPath, false)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return nil, err
	}
	defer unlockDirectory(directory)
	data, err := readPrivateRegularFileAt(directory, name)
	if err != nil {
		return nil, err
	}
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return nil, fmt.Errorf("consume private file: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return nil, fmt.Errorf("sync private directory after consume: %w", err)
	}
	return data, nil
}

// RemovePrivateFile removes one verified owner-only regular file. A missing
// file is a successful idempotent result.
func RemovePrivateFile(directoryPath string, name string) error {
	return RemovePrivateFileContext(context.Background(), directoryPath, name)
}

// RemovePrivateFileContext is RemovePrivateFile with cancelable lock
// acquisition.
func RemovePrivateFileContext(ctx context.Context, directoryPath string, name string) error {
	if ctx == nil {
		return errors.New("private file remove context is nil")
	}
	if err := validatePrivateFilePath(directoryPath, name); err != nil {
		return err
	}
	directory, err := openStateDirectory(directoryPath, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return err
	}
	defer unlockDirectory(directory)
	return removePrivateFileAt(directory, name)
}

func removePrivateFileAt(directory *os.File, name string) error {
	_, exists, err := inspectTargetAt(directory, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove private file: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync private directory: %w", err)
	}
	return nil
}

// LoadInto replaces target only after a complete strict decode and validation.
// On any error, target remains the caller's in-memory last known-good value.
func (file JSONFile[T]) LoadInto(target *T) error {
	return file.LoadIntoContext(context.Background(), target)
}

// LoadIntoContext is LoadInto with cancelable state-lock acquisition.
func (file JSONFile[T]) LoadIntoContext(ctx context.Context, target *T) error {
	if ctx == nil {
		return errors.New("state load context is nil")
	}
	if target == nil {
		return errors.New("state target is nil")
	}
	if err := file.validatePath(); err != nil {
		return err
	}
	directory, err := openStateDirectory(file.directory, false)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_SH); err != nil {
		return err
	}
	defer unlockDirectory(directory)
	return file.loadIntoAt(directory, target)
}

// LoadSnapshotInto reads one validated old-or-new view of an atomically
// replaced document without joining the directory lock. Use it only when a
// stale snapshot is safe and the caller must not wait behind a long external
// transaction. The descriptor-relative regular-file checks are unchanged.
func (file JSONFile[T]) LoadSnapshotInto(target *T) error {
	if target == nil {
		return errors.New("state target is nil")
	}
	if err := file.validatePath(); err != nil {
		return err
	}
	directory, err := openStateDirectory(file.directory, false)
	if err != nil {
		return err
	}
	defer directory.Close()
	return file.loadIntoAt(directory, target)
}

// Save validates and encodes a complete candidate before atomically replacing
// the existing file. Use Update for a read-modify-write operation that must not
// lose changes made by another process.
func (file JSONFile[T]) Save(value T) error {
	return file.SaveContext(context.Background(), value)
}

// SaveContext is Save with cancelable state-lock acquisition.
func (file JSONFile[T]) SaveContext(ctx context.Context, value T) error {
	if ctx == nil {
		return errors.New("state save context is nil")
	}
	if err := file.validatePath(); err != nil {
		return err
	}
	data, err := file.encode(value)
	if err != nil {
		return err
	}
	directory, err := openStateDirectory(file.directory, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return err
	}
	defer unlockDirectory(directory)
	return file.saveAt(directory, data)
}

// Update serializes a complete load-modify-save transaction with other users
// of this state directory. When the file does not exist, mutate receives
// initial. Existing malformed or invalid state fails closed before mutation.
// If the replacement becomes visible but its durability cannot be confirmed,
// Update returns the candidate with CommitOutcomeUnknownError so callers can
// reload and reconcile instead of assuming that the mutation was rolled back.
func (file JSONFile[T]) Update(initial T, mutate func(*T) error) (T, error) {
	return file.UpdateContext(context.Background(), initial, mutate)
}

// UpdateContext is Update with cancelable state-lock acquisition.
func (file JSONFile[T]) UpdateContext(ctx context.Context, initial T, mutate func(*T) error) (T, error) {
	if ctx == nil {
		return initial, errors.New("state update context is nil")
	}
	if mutate == nil {
		return initial, errors.New("state mutation is nil")
	}
	if err := file.validatePath(); err != nil {
		return initial, err
	}
	directory, err := openStateDirectory(file.directory, true)
	if err != nil {
		return initial, err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return initial, err
	}
	defer unlockDirectory(directory)

	candidate := initial
	if err := file.loadIntoAt(directory, &candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
		return initial, err
	}
	if err := mutate(&candidate); err != nil {
		return initial, fmt.Errorf("mutate %s: %w", file.name, err)
	}
	data, err := file.encode(candidate)
	if err != nil {
		return initial, err
	}
	if err := file.saveAt(directory, data); err != nil {
		var unknown *CommitOutcomeUnknownError
		if errors.As(err, &unknown) {
			return candidate, err
		}
		return initial, err
	}
	return candidate, nil
}

// InspectLocked loads the current document, or initial when it does not yet
// exist, and invokes inspect while holding the state-directory exclusive lock.
// It is intended for read-before-external-action workflows whose observation
// and side effects must not race a concurrent Update using the same directory.
// InspectLocked never writes the document itself.
func (file JSONFile[T]) InspectLocked(initial T, inspect func(T) error) error {
	return file.InspectLockedContext(context.Background(), initial, inspect)
}

// InspectLockedContext is InspectLocked with cancelable state-lock acquisition.
func (file JSONFile[T]) InspectLockedContext(ctx context.Context, initial T, inspect func(T) error) error {
	if ctx == nil {
		return errors.New("state inspection context is nil")
	}
	if inspect == nil {
		return errors.New("state inspection is nil")
	}
	if err := file.validatePath(); err != nil {
		return err
	}
	directory, err := openStateDirectory(file.directory, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return err
	}
	defer unlockDirectory(directory)

	candidate := initial
	if err := file.loadIntoAt(directory, &candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if file.validate != nil {
		if err := file.validate(&candidate); err != nil {
			return fmt.Errorf("validate %s: %w", file.name, err)
		}
	}
	return inspect(candidate)
}

func (file JSONFile[T]) loadIntoAt(directory *os.File, target *T) error {
	data, err := readPrivateRegularFileAt(directory, file.name)
	if err != nil {
		return err
	}
	var candidate T
	if err := decodeStrict(data, &candidate); err != nil {
		return fmt.Errorf("decode %s: %w", file.name, err)
	}
	if file.validate != nil {
		if err := file.validate(&candidate); err != nil {
			return fmt.Errorf("validate %s: %w", file.name, err)
		}
	}
	*target = candidate
	return nil
}

func (file JSONFile[T]) encode(value T) ([]byte, error) {
	if file.validate != nil {
		if err := file.validate(&value); err != nil {
			return nil, fmt.Errorf("validate %s: %w", file.name, err)
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", file.name, err)
	}
	data = append(data, '\n')
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("state file %s is too large: %d bytes exceeds %d", file.name, len(data), MaxFileSize)
	}
	return data, nil
}

func (file JSONFile[T]) saveAt(directory *os.File, data []byte) error {
	return savePrivateDataAt(directory, file.name, data, file.syncAfterRename)
}

func savePrivateDataAt(directory *os.File, name string, data []byte, syncAfterRename func(*os.File) error) error {
	previous, previousExists, err := inspectTargetAt(directory, name)
	if err != nil {
		return err
	}
	temporary, temporaryName, err := createTemporaryFileAt(directory, name)
	if err != nil {
		return err
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = unix.Unlinkat(int(directory.Fd()), temporaryName, 0)
		}
	}()

	if err := temporary.Chmod(RegularFileMode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary state permissions: %w", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary state file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := verifyUnchangedTargetAt(directory, name, previous, previousExists); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), temporaryName, int(directory.Fd()), name); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	keepTemporary = false
	if syncAfterRename == nil {
		syncAfterRename = syncFile
	}
	if err := syncAfterRename(directory); err != nil {
		return &CommitOutcomeUnknownError{Cause: fmt.Errorf("sync state directory: %w", err)}
	}
	return nil
}

func validatePrivateFilePath(directory string, name string) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("private directory must be a clean absolute path")
	}
	return validatePrivateFileName(name)
}

func validatePrivateFileName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return errors.New("private filename must be one base name")
	}
	return nil
}

func syncFile(file *os.File) error {
	return file.Sync()
}

func (file JSONFile[T]) validatePath() error {
	if file.directory == "" || !filepath.IsAbs(file.directory) {
		return errors.New("state directory must be an absolute path")
	}
	if filepath.Clean(file.directory) != file.directory {
		return errors.New("state directory must be a clean absolute path")
	}
	if file.name == "" || file.name == "." || file.name == ".." || filepath.Base(file.name) != file.name {
		return errors.New("state filename must be one base name")
	}
	return nil
}

func openStateDirectory(path string, create bool) (*os.File, error) {
	return openStateDirectoryWith(path, create, directoryOperations{
		mkdirAt: unix.Mkdirat,
		sync:    syncFile,
	})
}

type directoryOperations struct {
	mkdirAt func(int, string, uint32) error
	sync    func(*os.File) error
}

func openStateDirectoryWith(path string, create bool, operations directoryOperations) (*os.File, error) {
	rootFD, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	current := os.NewFile(uintptr(rootFD), string(os.PathSeparator))
	if current == nil {
		_ = unix.Close(rootFD)
		return nil, errors.New("open filesystem root: invalid file descriptor")
	}

	components := strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator))
	for _, component := range components {
		if component == "" {
			continue
		}
		info, statErr := current.Stat()
		if statErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("inspect ancestor of state directory component %s: %w", component, statErr)
		}
		if err := checkStateDirectoryAncestorMode(info.Mode()); err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("ancestor of state directory component %s: %w", component, err)
		}
		syncParent := false
		nextFD, openErr := openAt2(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := operations.mkdirAt(int(current.Fd()), component, uint32(DirectoryMode))
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = current.Close()
				return nil, fmt.Errorf("create state directory component %s: %w", component, mkdirErr)
			}
			// Another process can create the component between openat2 and
			// mkdirat. Every process that observed ENOENT must sync the parent
			// before reporting success; the creator may not have done so yet.
			syncParent = true
			nextFD, openErr = openAt2(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open state directory component %s: %w", component, openErr)
		}
		if syncParent {
			if syncErr := operations.sync(current); syncErr != nil {
				_ = unix.Close(nextFD)
				_ = current.Close()
				return nil, fmt.Errorf("sync parent of state directory component %s: %w", component, syncErr)
			}
		}
		next := os.NewFile(uintptr(nextFD), component)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, fmt.Errorf("open state directory component %s: invalid file descriptor", component)
		}
		nextInfo, statErr := next.Stat()
		if statErr != nil {
			_ = next.Close()
			_ = current.Close()
			return nil, fmt.Errorf("inspect state directory component %s: %w", component, statErr)
		}
		if err := checkStateDirectoryAncestorEntry(info, nextInfo); err != nil {
			_ = next.Close()
			_ = current.Close()
			return nil, fmt.Errorf("state directory component %s: %w", component, err)
		}
		_ = current.Close()
		current = next
	}

	info, err := current.Stat()
	if err != nil {
		_ = current.Close()
		return nil, fmt.Errorf("inspect state directory: %w", err)
	}
	if !info.IsDir() {
		_ = current.Close()
		return nil, errors.New("state directory must be a real directory")
	}
	if err := checkOwner(info); err != nil {
		_ = current.Close()
		return nil, fmt.Errorf("state directory: %w", err)
	}
	if err := checkMode(info, DirectoryMode); err != nil {
		_ = current.Close()
		return nil, fmt.Errorf("state directory: %w", err)
	}
	return current, nil
}

func openAt2(directoryFD int, name string, flags int, mode uint64) (int, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return -1, unix.EINVAL
	}
	fd, err := unix.Openat2(directoryFD, name, &unix.OpenHow{
		Flags:   uint64(flags),
		Mode:    mode,
		Resolve: safeResolveFlags,
	})
	if errors.Is(err, unix.ENOSYS) {
		return unix.Openat(directoryFD, name, flags|unix.O_NOFOLLOW, uint32(mode))
	}
	return fd, err
}

func readPrivateRegularFileAt(directory *os.File, name string) ([]byte, error) {
	fd, err := openAt2(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open state file: %w", err)
	}
	opened := os.NewFile(uintptr(fd), name)
	if opened == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open state file: invalid file descriptor")
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect state file: %w", err)
	}
	if err := checkRegularFile(info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(opened, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("state file is too large: exceeds %d bytes", MaxFileSize)
	}
	return data, nil
}

func inspectTargetAt(directory *os.File, name string) (os.FileInfo, bool, error) {
	fd, err := openAt2(int(directory.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect state file: %w", err)
	}
	opened := os.NewFile(uintptr(fd), name)
	if opened == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("inspect state file: invalid file descriptor")
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspect state file: %w", err)
	}
	if err := checkRegularFile(info); err != nil {
		return nil, false, err
	}
	return info, true, nil
}

func verifyUnchangedTargetAt(directory *os.File, name string, previous os.FileInfo, previousExists bool) error {
	current, exists, err := inspectTargetAt(directory, name)
	if err != nil {
		return err
	}
	if exists != previousExists {
		return errors.New("state file changed during atomic write")
	}
	if exists && !os.SameFile(previous, current) {
		return errors.New("state file was replaced during atomic write")
	}
	return nil
}

func createTemporaryFileAt(directory *os.File, targetName string) (*os.File, string, error) {
	for range temporaryNameAttempts {
		var entropy [12]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, "", fmt.Errorf("generate temporary state filename: %w", err)
		}
		name := "." + targetName + ".tmp-" + hex.EncodeToString(entropy[:])
		fd, err := openAt2(
			int(directory.Fd()),
			name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			uint64(RegularFileMode),
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create temporary state file: %w", err)
		}
		temporary := os.NewFile(uintptr(fd), name)
		if temporary == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(directory.Fd()), name, 0)
			return nil, "", errors.New("create temporary state file: invalid file descriptor")
		}
		return temporary, name, nil
	}
	return nil, "", errors.New("create temporary state file: too many filename collisions")
}

func lockDirectory(directory *os.File, operation int) error {
	if err := unix.Flock(int(directory.Fd()), operation); err != nil {
		return fmt.Errorf("lock state directory: %w", err)
	}
	return nil
}

func lockDirectoryContext(ctx context.Context, directory *os.File, operation int) error {
	if ctx == nil {
		return errors.New("state lock context is nil")
	}
	if ctx.Done() == nil {
		return lockDirectory(directory, operation)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("lock state directory: %w", err)
		}
		err := unix.Flock(int(directory.Fd()), operation|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock state directory: %w", err)
		}
		timer := time.NewTimer(lockRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return fmt.Errorf("lock state directory: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func unlockDirectory(directory *os.File) {
	_ = unix.Flock(int(directory.Fd()), unix.LOCK_UN)
}

func checkRegularFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("state file must be a regular file")
	}
	if err := checkOwner(info); err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	if err := checkMode(info, RegularFileMode); err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("state file ownership metadata is unavailable")
	}
	if stat.Nlink != 1 {
		return errors.New("state file must have exactly one hard link")
	}
	return nil
}

func checkOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("ownership metadata is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("must be owned by uid %d", os.Geteuid())
	}
	return nil
}

func checkMode(info os.FileInfo, expected os.FileMode) error {
	const special = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	actual := info.Mode() & (os.ModePerm | special)
	if actual != expected {
		return fmt.Errorf("must have mode %04o, got %04o", expected, actual)
	}
	return nil
}

func checkStateDirectoryAncestorMode(mode os.FileMode) error {
	if mode.Perm()&0o022 != 0 && mode&os.ModeSticky == 0 {
		return fmt.Errorf("must not be group- or world-writable without the sticky bit, got mode %04o", mode.Perm())
	}
	return nil
}

func checkStateDirectoryAncestorEntry(parent os.FileInfo, child os.FileInfo) error {
	if parent.Mode().Perm()&0o022 == 0 || parent.Mode()&os.ModeSticky == 0 {
		return nil
	}
	parentStat, parentOK := parent.Sys().(*syscall.Stat_t)
	childStat, childOK := child.Sys().(*syscall.Stat_t)
	if !parentOK || !childOK {
		return errors.New("sticky ancestor ownership metadata is unavailable")
	}
	userID := uint32(os.Geteuid())
	if !trustedStateDirectoryOwner(parentStat.Uid, userID) || !trustedStateDirectoryOwner(childStat.Uid, userID) {
		return fmt.Errorf("sticky writable ancestor and its entry must be owned by uid %d or root", userID)
	}
	return nil
}

func trustedStateDirectoryOwner(owner uint32, userID uint32) bool {
	return owner == 0 || owner == userID
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
