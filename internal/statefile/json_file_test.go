package statefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrivateFileContextOperationsStopWaitingForDirectoryLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	if err := CreatePrivateFile(root, "existing", []byte("data")); err != nil {
		t.Fatal(err)
	}
	directory, err := openStateDirectory(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := lockDirectory(directory, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unlockDirectory(directory)

	for name, operation := range map[string]func(context.Context) error{
		"compound": func(ctx context.Context) error {
			return WithPrivateDirectoryLockContext(ctx, root, func(*LockedPrivateDirectory) error {
				t.Fatal("compound action ran without acquiring the directory lock")
				return nil
			})
		},
		"create": func(ctx context.Context) error {
			return CreatePrivateFileContext(ctx, root, "new", []byte("data"))
		},
		"read": func(ctx context.Context) error {
			_, readErr := ReadPrivateFileContext(ctx, root, "existing")
			return readErr
		},
		"list": func(ctx context.Context) error {
			_, listErr := ListPrivateFilesContext(ctx, root, 2)
			return listErr
		},
		"consume": func(ctx context.Context) error {
			_, consumeErr := ConsumePrivateFileContext(ctx, root, "existing")
			return consumeErr
		},
		"remove": func(ctx context.Context) error {
			return RemovePrivateFileContext(ctx, root, "existing")
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := operation(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("locked %s returned %v, want context deadline", name, err)
			}
		})
	}
}

func TestOpenStateDirectorySyncsParentAfterConcurrentCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	wantSyncError := errors.New("parent sync failed")
	mkdirCalls := 0
	syncCalls := 0
	operations := directoryOperations{
		mkdirAt: func(directoryFD int, name string, mode uint32) error {
			mkdirCalls++
			if err := unix.Mkdirat(directoryFD, name, mode); err != nil {
				return err
			}
			// Model another process winning the race between the initial
			// openat2 and this process's mkdirat.
			return unix.EEXIST
		},
		sync: func(*os.File) error {
			syncCalls++
			return wantSyncError
		},
	}

	directory, err := openStateDirectoryWith(root, true, operations)
	if directory != nil {
		_ = directory.Close()
		t.Fatal("state directory was returned after its parent sync failed")
	}
	if !errors.Is(err, wantSyncError) {
		t.Fatalf("expected concurrent-creation parent sync error, got %v", err)
	}
	if mkdirCalls != 1 || syncCalls != 1 {
		t.Fatalf("unexpected operation counts: mkdir=%d sync=%d", mkdirCalls, syncCalls)
	}
}

func TestOpenStateDirectoryRejectsReplaceableAncestor(t *testing.T) {
	for name, mode := range map[string]os.FileMode{
		"group writable": 0o770,
		"world writable": 0o707,
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			ancestor := filepath.Join(base, "replaceable")
			if err := os.Mkdir(ancestor, DirectoryMode); err != nil {
				t.Fatalf("create ancestor: %v", err)
			}
			if err := os.Chmod(ancestor, mode); err != nil {
				t.Fatalf("set ancestor mode: %v", err)
			}
			root := filepath.Join(ancestor, "state")

			directory, err := OpenPrivateDirectory(root, true)
			if directory != nil {
				_ = directory.Close()
				t.Fatal("state directory was returned below a replaceable ancestor")
			}
			if err == nil || !strings.Contains(err.Error(), "group- or world-writable without the sticky bit") {
				t.Fatalf("expected replaceable-ancestor error, got %v", err)
			}
			if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("state directory was created below rejected ancestor: %v", statErr)
			}
		})
	}
}

func TestOpenStateDirectoryAllowsStickyAndNonWritableAncestors(t *testing.T) {
	base := t.TempDir()
	privateAncestor := filepath.Join(base, "private")
	if err := os.Mkdir(privateAncestor, DirectoryMode); err != nil {
		t.Fatalf("create private ancestor: %v", err)
	}
	stickyAncestor := filepath.Join(privateAncestor, "sticky")
	if err := os.Mkdir(stickyAncestor, DirectoryMode); err != nil {
		t.Fatalf("create sticky ancestor: %v", err)
	}
	if err := os.Chmod(stickyAncestor, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("set sticky ancestor mode: %v", err)
	}

	root := filepath.Join(stickyAncestor, "state")
	directory, err := OpenPrivateDirectory(root, true)
	if err != nil {
		t.Fatalf("open state directory below sticky and private ancestors: %v", err)
	}
	if err := directory.Close(); err != nil {
		t.Fatalf("close state directory: %v", err)
	}
}

func TestStateDirectoryAncestorModePolicy(t *testing.T) {
	for name, test := range map[string]struct {
		mode    os.FileMode
		wantErr bool
	}{
		"root style 0755":      {mode: 0o755},
		"user private 0700":    {mode: 0o700},
		"sticky tmp style":     {mode: 0o777 | os.ModeSticky},
		"group writable":       {mode: 0o770, wantErr: true},
		"world writable":       {mode: 0o707, wantErr: true},
		"group world writable": {mode: 0o777, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkStateDirectoryAncestorMode(test.mode)
			if (err != nil) != test.wantErr {
				t.Fatalf("check mode %v returned %v, wantErr=%v", test.mode, err, test.wantErr)
			}
		})
	}
}

func TestStickyStateDirectoryAncestorsTrustOnlyRootAndCurrentUser(t *testing.T) {
	const userID uint32 = 1000
	for name, test := range map[string]struct {
		parent uint32
		child  uint32
		valid  bool
	}{
		"root tmp and user entry":      {parent: 0, child: userID, valid: true},
		"user parent and user entry":   {parent: userID, child: userID, valid: true},
		"foreign parent and user leaf": {parent: 2000, child: userID},
		"user parent and foreign leaf": {parent: userID, child: 2000},
		"foreign parent and root leaf": {parent: 2000, child: 0},
	} {
		t.Run(name, func(t *testing.T) {
			valid := trustedStateDirectoryOwner(test.parent, userID) && trustedStateDirectoryOwner(test.child, userID)
			if valid != test.valid {
				t.Fatalf("trusted owner pair (%d, %d) = %t, want %t", test.parent, test.child, valid, test.valid)
			}
		})
	}
}

func TestStateAccessRejectsSymlinks(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		base := t.TempDir()
		realRoot := filepath.Join(base, "real")
		if err := os.Mkdir(realRoot, DirectoryMode); err != nil {
			t.Fatalf("create real directory: %v", err)
		}
		linkedRoot := filepath.Join(base, "linked")
		if err := os.Symlink(realRoot, linkedRoot); err != nil {
			t.Fatalf("create directory symlink: %v", err)
		}
		if err := CreatePrivateFile(linkedRoot, "state.json", []byte("unsafe")); err == nil {
			t.Fatal("expected symlink state directory to be rejected")
		}
	})

	t.Run("intermediate directory", func(t *testing.T) {
		base := t.TempDir()
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(outside, DirectoryMode); err != nil {
			t.Fatalf("create outside directory: %v", err)
		}
		linkedParent := filepath.Join(base, "linked")
		if err := os.Symlink(outside, linkedParent); err != nil {
			t.Fatalf("create intermediate symlink: %v", err)
		}
		if err := CreatePrivateFile(filepath.Join(linkedParent, "state"), "state.json", []byte("unsafe")); err == nil {
			t.Fatal("expected intermediate symlink to be rejected")
		}
		if _, err := os.Lstat(filepath.Join(outside, "state")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("state was created through an intermediate symlink: %v", err)
		}
	})

	t.Run("file", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "state")
		if err := os.Mkdir(root, DirectoryMode); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		outside := filepath.Join(base, "outside.json")
		if err := os.WriteFile(outside, []byte("outside"), RegularFileMode); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "state.json")); err != nil {
			t.Fatalf("create state symlink: %v", err)
		}
		if _, err := ReadPrivateFile(root, "state.json"); err == nil {
			t.Fatal("expected symlink state file to be rejected")
		}
		contents, err := os.ReadFile(outside)
		if err != nil {
			t.Fatalf("read outside file: %v", err)
		}
		if string(contents) != "outside" {
			t.Fatalf("symlink target was modified: %q", contents)
		}
	})
}

func TestOpenedDirectoryFDDoesNotFollowReplacementSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "state")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(root, DirectoryMode); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := os.Mkdir(outside, DirectoryMode); err != nil {
		t.Fatalf("create outside directory: %v", err)
	}

	directory, err := openStateDirectory(root, false)
	if err != nil {
		t.Fatalf("open state directory: %v", err)
	}
	defer directory.Close()
	heldRoot := filepath.Join(base, "held-state")
	if err := os.Rename(root, heldRoot); err != nil {
		t.Fatalf("move opened state directory: %v", err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatalf("replace state path with symlink: %v", err)
	}

	if err := CreatePrivateFileAt(directory, "state.json", []byte("safe")); err != nil {
		t.Fatalf("save through held directory descriptor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement symlink received state data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(heldRoot, "state.json")); err != nil {
		t.Fatalf("held state directory did not receive state data: %v", err)
	}
}

func TestStateAccessRejectsUnexpectedModesAndFileTypes(t *testing.T) {
	t.Run("directory mode", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		if err := CreatePrivateFile(root, "state.json", []byte("unsafe")); err == nil {
			t.Fatal("expected public state directory to be rejected")
		}
	})

	t.Run("file mode", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, DirectoryMode); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		path := filepath.Join(root, "state.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"value":"unsafe"}`), 0o644); err != nil {
			t.Fatalf("write state file: %v", err)
		}
		if _, err := ReadPrivateFile(root, "state.json"); err == nil {
			t.Fatal("expected public state file to be rejected")
		}
	})

	t.Run("target directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, DirectoryMode); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		if err := os.Mkdir(filepath.Join(root, "state.json"), RegularFileMode); err != nil {
			t.Fatalf("create unexpected target: %v", err)
		}
		if _, err := ReadPrivateFile(root, "state.json"); err == nil {
			t.Fatal("expected non-regular target to be rejected")
		}
	})

	t.Run("fifo read does not block", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, DirectoryMode); err != nil {
			t.Fatalf("create state directory: %v", err)
		}
		if err := unix.Mkfifo(filepath.Join(root, "state.json"), uint32(RegularFileMode)); err != nil {
			t.Fatalf("create state fifo: %v", err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := ReadPrivateFile(root, "state.json")
			finished <- err
		}()
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("expected fifo state file to be rejected")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("loading a fifo blocked before its file type could be rejected")
		}
	})
}

func TestPrivateFileRejectsUnsafePaths(t *testing.T) {
	for _, test := range []struct {
		directory string
		name      string
	}{
		{directory: "relative/state", name: "state.json"},
		{directory: t.TempDir() + string(os.PathSeparator) + "state" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "other", name: "state.json"},
		{directory: filepath.Join(t.TempDir(), "state"), name: "../state.json"},
	} {
		if err := CreatePrivateFile(test.directory, test.name, []byte("unsafe")); err == nil {
			t.Fatal("expected unsafe private file path to be rejected")
		}
	}
}

func TestCheckOwnerRejectsDifferentUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("state"), RegularFileMode); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("inspect state file: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("ownership metadata is unavailable")
	}
	differentOwner := *stat
	differentOwner.Uid ^= 1
	wrapped := fileInfoWithSystem{FileInfo: info, system: &differentOwner}
	if err := checkOwner(wrapped); err == nil {
		t.Fatal("expected a state file owned by another UID to be rejected")
	}
}

type fileInfoWithSystem struct {
	os.FileInfo
	system any
}

func (info fileInfoWithSystem) Sys() any {
	return info.system
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("inspect %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("unexpected mode for %s: got=%04o want=%04o", path, got, want)
	}
}

func TestPrivateFileCreateConsumeAndReplayProtection(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := CreatePrivateFile(directory, "token.json", []byte("typed operation")); err != nil {
		t.Fatal(err)
	}
	assertMode(t, directory, DirectoryMode)
	assertMode(t, filepath.Join(directory, "token.json"), RegularFileMode)
	if err := CreatePrivateFile(directory, "token.json", []byte("replacement")); err == nil {
		t.Fatal("exclusive private file was replaced")
	}
	data, err := ConsumePrivateFile(directory, "token.json")
	if err != nil || string(data) != "typed operation" {
		t.Fatalf("consume failed data=%q err=%v", data, err)
	}
	if _, err := ConsumePrivateFile(directory, "token.json"); err == nil {
		t.Fatal("private file was replayable")
	}
}

func TestPrivateFileCreateReportsVisibleUnknownOutcomeWhenDirectorySyncFails(t *testing.T) {
	directoryPath := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directoryPath, DirectoryMode); err != nil {
		t.Fatal(err)
	}
	directory, err := OpenPrivateDirectory(directoryPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	want := errors.New("directory sync failed")
	err = createPrivateFileAtWithDirectorySync(directory, "state.sqlite3", nil, func() error { return want })
	var unknown *CommitOutcomeUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, want) {
		t.Fatalf("directory sync failure returned %v", err)
	}
	info, statErr := os.Stat(filepath.Join(directoryPath, "state.sqlite3"))
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != RegularFileMode {
		t.Fatalf("visible created file was removed after unknown outcome: info=%v err=%v", info, statErr)
	}
}

func TestPrivateFileRejectsSymlinkAndFIFO(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, DirectoryMode); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), RegularFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(directory, "link"); err == nil {
		t.Fatal("private file read followed symlink")
	}
	if err := unix.Mkfifo(filepath.Join(directory, "fifo"), uint32(RegularFileMode)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ConsumePrivateFile(directory, "fifo")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("private file consume accepted FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("private file consume blocked on FIFO")
	}
}

func TestLockedPrivateDirectoryOperationsRemainOnVerifiedInodeAfterPathReplacement(t *testing.T) {
	root := t.TempDir()
	directoryPath := filepath.Join(root, "private")
	displacedPath := filepath.Join(root, "displaced")

	err := WithPrivateDirectoryLock(directoryPath, func(directory *LockedPrivateDirectory) error {
		if err := os.Rename(directoryPath, displacedPath); err != nil {
			return fmt.Errorf("displace locked directory: %w", err)
		}
		if err := os.Mkdir(directoryPath, DirectoryMode); err != nil {
			return fmt.Errorf("replace locked directory path: %w", err)
		}
		if err := directory.Create("token.json", []byte("pinned inode")); err != nil {
			return err
		}
		names, err := directory.List(1)
		if err != nil {
			return err
		}
		if len(names) != 1 || names[0] != "token.json" {
			return fmt.Errorf("locked listing used replacement path: %v", names)
		}
		data, err := directory.Read("token.json")
		if err != nil {
			return err
		}
		if string(data) != "pinned inode" {
			return fmt.Errorf("locked read returned %q", data)
		}
		return directory.Remove("token.json")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directoryPath, "token.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement path unexpectedly contains token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(displacedPath, "token.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor-relative remove did not remove token: %v", err)
	}
}
