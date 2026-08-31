package statefile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// MigrationDecoder strictly decodes either the current representation or one
// supported legacy representation. migrated is true only when the returned
// value must replace the source bytes.
type MigrationDecoder[T any] func([]byte) (value T, migrated bool, err error)

// Migrate atomically replaces one supported legacy document after preserving
// its exact bytes in a dedicated owner-only sibling backup. This code never
// replaces differing backup bytes or uses the backup as an automatic fallback.
func (file JSONFile[T]) Migrate(decoder MigrationDecoder[T], backupName string) (T, bool, error) {
	return file.MigrateContext(context.Background(), decoder, backupName)
}

// MigrateContext is Migrate with cancelable exclusive-lock acquisition.
func (file JSONFile[T]) MigrateContext(ctx context.Context, decoder MigrationDecoder[T], backupName string) (T, bool, error) {
	var zero T
	if ctx == nil {
		return zero, false, errors.New("state migration context is nil")
	}
	if decoder == nil {
		return zero, false, errors.New("state migration decoder is nil")
	}
	if err := file.validatePath(); err != nil {
		return zero, false, err
	}
	if err := validateSiblingName(backupName); err != nil {
		return zero, false, fmt.Errorf("invalid migration backup name: %w", err)
	}
	if backupName == file.name {
		return zero, false, errors.New("migration backup must differ from the state filename")
	}

	directory, err := openStateDirectory(file.directory, false)
	if err != nil {
		return zero, false, err
	}
	defer directory.Close()
	if err := lockDirectoryContext(ctx, directory, unix.LOCK_EX); err != nil {
		return zero, false, err
	}
	defer unlockDirectory(directory)

	source, err := readPrivateRegularFileAt(directory, file.name)
	if err != nil {
		return zero, false, err
	}
	candidate, migrated, err := decoder(source)
	if err != nil {
		return zero, false, fmt.Errorf("decode migration source %s: %w", file.name, err)
	}
	encoded, err := file.encode(candidate)
	if err != nil {
		return zero, false, err
	}
	if !migrated {
		return candidate, false, nil
	}
	if err := preserveMigrationBackupAt(directory, backupName, source); err != nil {
		return zero, false, fmt.Errorf("preserve migration backup %s: %w", backupName, err)
	}
	if err := file.saveAt(directory, encoded); err != nil {
		var unknown *CommitOutcomeUnknownError
		if errors.As(err, &unknown) {
			return candidate, true, err
		}
		return zero, false, err
	}
	return candidate, true, nil
}

func preserveMigrationBackupAt(directory *os.File, name string, data []byte) error {
	if existing, err := readPrivateRegularFileAt(directory, name); err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("existing migration backup differs from legacy state")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
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
		return fmt.Errorf("set migration backup permissions: %w", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write migration backup: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync migration backup: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close migration backup: %w", err)
	}

	err = unix.Renameat2(int(directory.Fd()), temporaryName, int(directory.Fd()), name, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		existing, readErr := readPrivateRegularFileAt(directory, name)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, data) {
			return errors.New("concurrent migration backup differs from legacy state")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("install migration backup: %w", err)
	}
	keepTemporary = false
	if err := directory.Sync(); err != nil {
		return &CommitOutcomeUnknownError{Cause: fmt.Errorf("sync migration backup directory: %w", err)}
	}
	return nil
}

func validateSiblingName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return errors.New("filename must be one base name")
	}
	return nil
}
