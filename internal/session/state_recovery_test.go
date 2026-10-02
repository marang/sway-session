package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Recovery tests use the public stores for their data oracle. Filesystem
// observations below concern the recovery contract: no preview/rejection
// writes, exact preservation of old bytes, and retries after process death.
type recoveryTestState struct {
	registry Registry
	layout   LayoutSnapshot
	report   RestoreReport
}

func recoveryTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func recoveryTestSeed(t *testing.T, root, label string, ids ...ContextID) recoveryTestState {
	t.Helper()
	state := recoveryTestState{
		registry: Registry{Version: ContextsSchemaVersion, Contexts: []Context{}},
		layout:   placementSnapshot("98: "+label, ids...),
	}
	if len(ids) == 2 {
		state.layout = twoChildLayout(ids[0], ids[1])
		state.layout.Workspaces[0].Name = "98: " + label
	}
	for i, id := range ids {
		entry := validRegistry().Contexts[0]
		entry.ID = id
		entry.Label = fmt.Sprintf("%s-%d", label, i)
		entry.Launcher.Session = fmt.Sprintf("recovery-%s-%d", label, i)
		state.registry.Contexts = append(state.registry.Contexts, entry)
	}
	if err := RegistryStoreFor(root).Save(state.registry); err != nil {
		t.Fatal(err)
	}
	if err := LayoutStoreFor(root).Save(state.layout); err != nil {
		t.Fatal(err)
	}
	automatic := restoreTestOutcome("automatic", ids[0])
	automatic.Requested.Workspace = "98: " + label
	explicit := restoreTestOutcome("explicit", ids[len(ids)-1])
	explicit.Requested.Workspace = "98: " + label
	explicit.AttemptID = restoreTestOtherAttempt
	explicit.Status, explicit.Reason, explicit.WindowMapped = "completed", "restore_complete", true
	store := RestoreReportStoreFor(root)
	if err := store.BeginAutomaticContext(t.Context(), restoreTestRun, []RestoreOutcome{automatic}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginExplicitContext(t.Context(), []RestoreOutcome{explicit}); err != nil {
		t.Fatal(err)
	}
	state.report = RestoreReport{AutomaticRunID: restoreTestRun, Outcomes: []RestoreOutcome{automatic, explicit}}
	return state
}

func recoveryTestSource(t *testing.T) (string, recoveryTestState) {
	t.Helper()
	root := recoveryTestDir(t)
	want := recoveryTestSeed(t, root, "incoming", testContextID, secondContextID)
	source := filepath.Join(recoveryTestDir(t), "backup.sqlite3")
	if _, err := BackupState(t.Context(), root, source); err != nil {
		t.Fatal(err)
	}
	return source, want
}

func recoveryTestAssertState(t *testing.T, root string, want recoveryTestState) {
	t.Helper()
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
		t.Fatalf("read recovered registry: %v", err)
	}
	if !reflect.DeepEqual(registry, want.registry) {
		t.Errorf("registry = %+v, want %+v", registry, want.registry)
	}
	var layout LayoutSnapshot
	if err := LayoutStoreFor(root).LoadInto(&layout); err != nil {
		t.Fatalf("read recovered layout: %v", err)
	}
	if !reflect.DeepEqual(layout, want.layout) {
		t.Errorf("layout = %+v, want %+v", layout, want.layout)
	}
	report, err := RestoreReportStoreFor(root).LoadContext(t.Context())
	if err != nil {
		t.Fatalf("read recovered report: %v", err)
	}
	if !reflect.DeepEqual(report, want.report) {
		t.Errorf("report = %+v, want %+v", report, want.report)
	}
}

type recoveryTestObject struct {
	Mode    fs.FileMode
	ModTime time.Time
	Device  uint64
	Inode   uint64
	Links   uint64
	UID     uint32
	Data    string
}

func recoveryTestSnapshot(t *testing.T, root string) map[string]recoveryTestObject {
	t.Helper()
	objects := make(map[string]recoveryTestObject)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == root {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return err
		}
		object := recoveryTestObject{Mode: info.Mode(), ModTime: info.ModTime(), Device: stat.Dev, Inode: stat.Ino, Links: stat.Nlink, UID: stat.Uid}
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			object.Data = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			object.Data, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		objects[relative] = object
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

func recoveryTestUnchanged(t *testing.T, root string, before map[string]recoveryTestObject) {
	t.Helper()
	after := recoveryTestSnapshot(t, root)
	for name, old := range before {
		if got, ok := after[name]; !ok || got != old {
			t.Errorf("recovery changed %s (exists=%v, bytes equal=%v, mode=%v, inode=%d)", filepath.Join(root, name), ok, got.Data == old.Data, got.Mode, got.Inode)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("recovery created %s", filepath.Join(root, name))
		}
	}
}

func recoveryTestRawBundle(t *testing.T, root string) map[string]recoveryTestObject {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data := bytes.Repeat([]byte("unreadable old database"+suffix+"\x00\xff"), 100)
		if err := os.WriteFile(filepath.Join(root, StateDatabaseFilename+suffix), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return recoveryTestDatabaseObjects(t, root)
}

func recoveryTestDatabaseObjects(t *testing.T, root string) map[string]recoveryTestObject {
	t.Helper()
	objects := recoveryTestSnapshot(t, root)
	selected := make(map[string]recoveryTestObject)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		name := StateDatabaseFilename + suffix
		if object, ok := objects[name]; ok {
			selected[name] = object
		}
	}
	return selected
}

func recoveryTestAssertArchive(t *testing.T, root string, old map[string]recoveryTestObject) {
	t.Helper()
	objects := recoveryTestSnapshot(t, root)
	for name, want := range old {
		matches := 0
		for path, got := range objects {
			if path == name || filepath.Base(path) != name {
				continue
			}
			matches++
			if got != want {
				t.Errorf("archived %s did not preserve old bytes/identity/mode (bytes equal=%v)", name, got.Data == want.Data)
			}
		}
		if matches != 1 {
			t.Errorf("found %d archived copies of %s, want exactly one", matches, name)
		}
	}
}

func recoveryTestPending(t *testing.T, root string) {
	t.Helper()
	before := recoveryTestSnapshot(t, root)
	var registry Registry
	if err := RegistryStoreFor(root).LoadInto(&registry); !errors.Is(err, ErrStateRecoveryPending) {
		t.Errorf("ordinary registry read during recovery = %v, want ErrStateRecoveryPending", err)
	}
	if err := RegistryStoreFor(root).Save(emptyRegistry()); !errors.Is(err, ErrStateRecoveryPending) {
		t.Errorf("ordinary registry write during recovery = %v, want ErrStateRecoveryPending", err)
	}
	recoveryTestUnchanged(t, root, before)
}

func recoveryTestRepeat(t *testing.T, root, source string, first StateRecoveryResult) {
	t.Helper()
	before := recoveryTestSnapshot(t, root)
	for range 2 {
		result, err := RecoverState(t.Context(), root, source, true)
		if err != nil {
			t.Fatalf("retry completed recovery: %v", err)
		}
		if !result.Applied || !result.Resumed || result.Previous != first.Previous || result.PreviousValid != first.PreviousValid || result.Source != source || result.Database != first.Database {
			t.Errorf("retry changed recovery result: first=%+v retry=%+v", first, result)
		}
		recoveryTestUnchanged(t, root, before)
	}
}

func TestRecoverStateRoundTrip(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "healthy", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			sourceBefore := recoveryTestSnapshot(t, filepath.Dir(source))
			root := filepath.Join(recoveryTestDir(t), "target")
			var previous recoveryTestState
			var old map[string]recoveryTestObject
			switch kind {
			case "empty":
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			case "healthy":
				previous = recoveryTestSeed(t, root, "previous", thirdContextID)
				old = recoveryTestDatabaseObjects(t, root)
			case "corrupt":
				old = recoveryTestRawBundle(t, root)
			}
			result, err := RecoverState(t.Context(), root, source, true)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Applied || result.Resumed || result.Source != source || result.Database != filepath.Join(root, StateDatabaseFilename) {
				t.Fatalf("incorrect recovery result: %+v", result)
			}
			if result.PreviousValid != (kind == "healthy") || (result.Previous != "") != (len(old) != 0) {
				t.Fatalf("incorrect previous-state result: %+v", result)
			}
			if len(old) != 0 {
				recoveryTestAssertArchive(t, root, old)
			}
			if kind == "corrupt" && !reflect.DeepEqual(recoveryTestDatabaseObjects(t, result.Previous), old) {
				t.Error("Previous does not name the preserved raw bundle")
			}
			for path, object := range recoveryTestSnapshot(t, root) {
				mode := fs.FileMode(0o600)
				if object.Mode.IsDir() {
					mode = 0o700
				}
				if object.Mode.Perm() != mode || object.UID != uint32(os.Geteuid()) {
					t.Errorf("recovery output %s is not owner-only: mode=%v uid=%d", path, object.Mode, object.UID)
				}
			}
			recoveryTestRepeat(t, root, source, result)
			recoveryTestAssertState(t, root, want)
			recoveryTestUnchanged(t, filepath.Dir(source), sourceBefore)
			if kind == "healthy" {
				if _, err := ValidateStateBackup(t.Context(), result.Previous); err != nil {
					t.Fatalf("PreviousValid backup is not standalone: %v", err)
				}
				restored := filepath.Join(recoveryTestDir(t), "previous-restored")
				if _, err := RecoverState(t.Context(), restored, result.Previous, true); err != nil {
					t.Fatalf("recover previous backup: %v", err)
				}
				recoveryTestAssertState(t, restored, previous)
			}
		})
	}
}

func TestRecoverStatePreviewIsReadOnly(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "healthy", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			source, _ := recoveryTestSource(t)
			parent := recoveryTestDir(t)
			root := filepath.Join(parent, "target")
			switch kind {
			case "empty":
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			case "healthy":
				recoveryTestSeed(t, root, "previous", thirdContextID)
			case "corrupt":
				recoveryTestRawBundle(t, root)
			}
			before, inputBefore := recoveryTestSnapshot(t, parent), recoveryTestSnapshot(t, filepath.Dir(source))
			result, err := RecoverState(t.Context(), root, source, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Applied || result.Resumed || result.Previous != "" || result.PreviousValid || result.Source != source || result.Database != filepath.Join(root, StateDatabaseFilename) {
				t.Errorf("preview claimed effects: %+v", result)
			}
			recoveryTestUnchanged(t, parent, before)
			recoveryTestUnchanged(t, filepath.Dir(source), inputBefore)
		})
	}
}

func TestRecoverStateCanceledBeforeEffects(t *testing.T) {
	for _, apply := range []bool{false, true} {
		for _, kind := range []string{"missing", "healthy", "corrupt"} {
			t.Run(fmt.Sprintf("%s/apply=%v", kind, apply), func(t *testing.T) {
				source, _ := recoveryTestSource(t)
				parent := recoveryTestDir(t)
				root := filepath.Join(parent, "target")
				if kind == "healthy" {
					recoveryTestSeed(t, root, "previous", thirdContextID)
				} else if kind == "corrupt" {
					recoveryTestRawBundle(t, root)
				}
				before, inputBefore := recoveryTestSnapshot(t, parent), recoveryTestSnapshot(t, filepath.Dir(source))
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				result, err := RecoverState(ctx, root, source, apply)
				if !errors.Is(err, context.Canceled) || result.Applied {
					t.Errorf("canceled recovery = %+v, %v", result, err)
				}
				recoveryTestUnchanged(t, parent, before)
				recoveryTestUnchanged(t, filepath.Dir(source), inputBefore)
			})
		}
	}
}

func TestRecoverStateRejectsInvalidSourcesWithoutEffects(t *testing.T) {
	for _, kind := range []string{
		"corrupt", "truncated", "newer schema", "missing", "symlink", "hardlink", "fifo", "directory",
		"public file", "public parent", "symlink parent", "relative", "unclean", "wal", "shm", "journal",
	} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/apply=%v", kind, apply), func(t *testing.T) {
				source, _ := recoveryTestSource(t)
				inputParent := filepath.Dir(source)
				root := recoveryTestDir(t)
				recoveryTestSeed(t, root, "previous", thirdContextID)
				var setupErr error
				switch kind {
				case "corrupt":
					setupErr = os.WriteFile(source, []byte("not a SQLite database"), 0o600)
				case "truncated":
					setupErr = os.Truncate(source, 100)
				case "newer schema":
					data, err := os.ReadFile(source)
					if err != nil {
						t.Fatal(err)
					}
					// SQLite's user_version header field is independent of the
					// recovery implementation and leaves a structurally valid DB.
					binary.BigEndian.PutUint32(data[60:64], 0x7fffffff)
					setupErr = os.WriteFile(source, data, 0o600)
				case "missing", "fifo", "directory":
					if err := os.Remove(source); err != nil {
						t.Fatal(err)
					}
					if kind == "fifo" {
						setupErr = unix.Mkfifo(source, 0o600)
					} else if kind == "directory" {
						setupErr = os.Mkdir(source, 0o700)
					}
				case "symlink", "hardlink":
					alias := filepath.Join(inputParent, "alias.sqlite3")
					if kind == "symlink" {
						setupErr = os.Symlink(source, alias)
					} else {
						setupErr = os.Link(source, alias)
					}
					source = alias
				case "public file":
					setupErr = os.Chmod(source, 0o644)
				case "public parent":
					setupErr = os.Chmod(inputParent, 0o755)
				case "symlink parent":
					link := filepath.Join(recoveryTestDir(t), "alias")
					setupErr = os.Symlink(inputParent, link)
					source = filepath.Join(link, filepath.Base(source))
				case "relative":
					cwd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					source, setupErr = filepath.Rel(cwd, source)
				case "unclean":
					source = inputParent + "/./backup.sqlite3"
				case "wal", "shm", "journal":
					setupErr = os.WriteFile(source+"-"+kind, []byte("uncheckpointed input"), 0o600)
				}
				if setupErr != nil {
					t.Fatal(setupErr)
				}
				before, inputBefore := recoveryTestSnapshot(t, root), recoveryTestSnapshot(t, inputParent)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				result, err := RecoverState(ctx, root, source, apply)
				if err == nil || result.Applied || result.Previous != "" {
					t.Errorf("invalid input accepted: %+v, %v", result, err)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("invalid input was not rejected before the deadline: %v", err)
				}
				if kind == "newer schema" {
					var version *UnsupportedVersionError
					if !errors.As(err, &version) || version.Got != 0x7fffffff {
						t.Errorf("unsupported schema was not identified: %v", err)
					}
				}
				recoveryTestUnchanged(t, root, before)
				recoveryTestUnchanged(t, inputParent, inputBefore)
			})
		}
	}
}

func TestRecoverStateRejectsAliasesOfCurrentDatabaseAndSidecars(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("state.sqlite3%s/apply=%v", suffix, apply), func(t *testing.T) {
				backup, _ := recoveryTestSource(t)
				root := recoveryTestDir(t)
				// A valid standalone image in a reserved live name must still
				// be rejected. This cannot pass solely through corrupt-input checks.
				source := filepath.Join(root, StateDatabaseFilename+suffix)
				if err := os.Rename(backup, source); err != nil {
					t.Fatal(err)
				}
				if _, err := ValidateStateBackup(t.Context(), source); err != nil {
					t.Fatalf("alias fixture is not independently valid: %v", err)
				}
				before := recoveryTestSnapshot(t, root)
				result, err := RecoverState(t.Context(), root, source, apply)
				if err == nil || result.Applied || result.Previous != "" {
					t.Errorf("live alias accepted: %+v, %v", result, err)
				}
				if !apply {
					recoveryTestUnchanged(t, root, before)
				} else if got := recoveryTestDatabaseObjects(t, root); len(got) != 1 || got[filepath.Base(source)] != before[filepath.Base(source)] {
					t.Error("rejected alias changed live database entries")
				}
			})
		}
	}
}

func TestRecoverStateRejectsUnsafeRootsWithoutEffects(t *testing.T) {
	for _, kind := range []string{"symlink", "public directory", "relative", "unclean", "empty", "nil context"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/apply=%v", kind, apply), func(t *testing.T) {
				source, _ := recoveryTestSource(t)
				parent := recoveryTestDir(t)
				root := filepath.Join(parent, "target")
				recoveryTestSeed(t, root, "previous", thirdContextID)
				ctx := t.Context()
				var err error
				switch kind {
				case "symlink":
					alias := filepath.Join(parent, "alias")
					err = os.Symlink(root, alias)
					root = alias
				case "public directory":
					err = os.Chmod(root, 0o755)
				case "relative":
					cwd, cwdErr := os.Getwd()
					if cwdErr != nil {
						t.Fatal(cwdErr)
					}
					root, err = filepath.Rel(cwd, root)
				case "unclean":
					root = parent + "/./target"
				case "empty":
					root = ""
				case "nil context":
					ctx = nil
				}
				if err != nil {
					t.Fatal(err)
				}
				before, inputBefore := recoveryTestSnapshot(t, parent), recoveryTestSnapshot(t, filepath.Dir(source))
				if result, err := RecoverState(ctx, root, source, apply); err == nil || result.Applied {
					t.Errorf("unsafe root accepted: %+v, %v", result, err)
				}
				recoveryTestUnchanged(t, parent, before)
				recoveryTestUnchanged(t, filepath.Dir(source), inputBefore)
			})
		}
	}
}

func TestRecoverStateRejectsUnsafeCurrentDatabaseObjects(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "public file"} {
			t.Run("state.sqlite3"+suffix+"/"+kind, func(t *testing.T) {
				source, _ := recoveryTestSource(t)
				root := recoveryTestDir(t)
				recoveryTestRawBundle(t, root)
				destination := filepath.Join(root, StateDatabaseFilename+suffix)
				if err := os.Remove(destination); err != nil {
					t.Fatal(err)
				}
				foreign := recoveryTestDir(t)
				target := filepath.Join(foreign, "untouched")
				if err := os.WriteFile(target, []byte("unrelated owner data"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(target, destination)
				case "hardlink":
					err = os.Link(target, destination)
				case "fifo":
					err = unix.Mkfifo(destination, 0o600)
				case "directory":
					err = os.Mkdir(destination, 0o700)
				case "public file":
					err = os.WriteFile(destination, []byte("unsafe mode"), 0o644)
					if err == nil {
						err = os.Chmod(destination, 0o644)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				old := recoveryTestDatabaseObjects(t, root)
				foreignBefore, inputBefore := recoveryTestSnapshot(t, foreign), recoveryTestSnapshot(t, filepath.Dir(source))
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				result, err := RecoverState(ctx, root, source, true)
				if err == nil || result.Applied || result.Previous != "" {
					t.Errorf("unsafe current object accepted: %+v, %v", result, err)
				}
				if !reflect.DeepEqual(recoveryTestDatabaseObjects(t, root), old) {
					t.Error("rejected recovery changed current database objects")
				}
				recoveryTestUnchanged(t, foreign, foreignBefore)
				recoveryTestUnchanged(t, filepath.Dir(source), inputBefore)
			})
		}
	}
}

func TestRecoverStatePendingRejectsDifferentInputAndPreviewDoesNotResume(t *testing.T) {
	source, want := recoveryTestSource(t)
	otherRoot := recoveryTestDir(t)
	recoveryTestSeed(t, otherRoot, "different", thirdContextID)
	other := filepath.Join(recoveryTestDir(t), "other.sqlite3")
	if _, err := BackupState(t.Context(), otherRoot, other); err != nil {
		t.Fatal(err)
	}
	root := recoveryTestDir(t)
	recoveryTestRawBundle(t, root)
	failure := errors.New("interrupt before installation")
	hit := recoveryTestFault(t, "archived-state.sqlite3", failure)
	if _, err := RecoverState(t.Context(), root, source, true); !*hit || !errors.Is(err, failure) {
		t.Fatalf("could not interrupt recovery: %v", err)
	}
	recoveryBoundary = func(string) error { return nil }
	before := recoveryTestSnapshot(t, root)
	if result, err := RecoverState(t.Context(), root, other, true); !errors.Is(err, ErrStateRecoveryPending) || result.Applied {
		t.Errorf("different input changed pending operation: %+v, %v", result, err)
	}
	if result, err := RecoverState(t.Context(), root, source, false); err != nil || result.Applied {
		t.Errorf("pending preview = %+v, %v", result, err)
	}
	recoveryTestUnchanged(t, root, before)
	recoveryTestPending(t, root)
	result, err := RecoverState(t.Context(), root, source, true)
	if err != nil || !result.Applied || !result.Resumed {
		t.Fatalf("same-input retry did not resume: %+v, %v", result, err)
	}
	recoveryTestAssertState(t, root, want)
}

func TestRecoverStatePreservesHealthyUncheckpointedWAL(t *testing.T) {
	source, want := recoveryTestSource(t)
	root := recoveryTestDir(t)
	previous := recoveryTestSeed(t, root, "previous", thirdContextID)
	recoveryTestRunChild(t, root, source, "leave-committed-wal")
	previous.registry.Preferences.DesktopIndicators = true
	old := recoveryTestDatabaseObjects(t, root)
	if len(old[StateDatabaseFilename+"-wal"].Data) <= 32 || len(old[StateDatabaseFilename+"-shm"].Data) == 0 {
		t.Fatal("subprocess did not leave committed WAL and SHM")
	}
	result, err := RecoverState(t.Context(), root, source, true)
	if err != nil || !result.Applied || !result.PreviousValid || result.Previous == "" {
		t.Fatalf("healthy WAL recovery = %+v, %v", result, err)
	}
	recoveryTestAssertArchive(t, root, old)
	if _, err := ValidateStateBackup(t.Context(), result.Previous); err != nil {
		t.Fatalf("normalized previous WAL snapshot is not standalone: %v", err)
	}
	previousBefore := recoveryTestSnapshot(t, filepath.Dir(result.Previous))
	restored := filepath.Join(recoveryTestDir(t), "restored-previous")
	if _, err := RecoverState(t.Context(), restored, result.Previous, true); err != nil {
		t.Fatalf("recover previous WAL snapshot: %v", err)
	}
	recoveryTestAssertState(t, restored, previous)
	recoveryTestUnchanged(t, filepath.Dir(result.Previous), previousBefore)
	recoveryTestRepeat(t, root, source, result)
	recoveryTestAssertState(t, root, want)
}

func TestRecoverStateCompletedRetryRejectsDamagedPrevious(t *testing.T) {
	for _, kind := range []string{"raw archive", "validated previous backup", "missing previous backup"} {
		t.Run(kind, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			if kind == "raw archive" {
				recoveryTestRawBundle(t, root)
			} else {
				recoveryTestSeed(t, root, "previous", thirdContextID)
			}
			first, err := RecoverState(t.Context(), root, source, true)
			if err != nil {
				t.Fatal(err)
			}
			if first.Previous == "" {
				t.Fatal("recovery did not preserve previous state")
			}
			target := first.Previous
			if kind == "raw archive" {
				target = filepath.Join(target, StateDatabaseFilename)
			}
			if kind == "missing previous backup" {
				err = os.Remove(target)
			} else {
				err = os.WriteFile(target, []byte("damaged after successful recovery"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := recoveryTestSnapshot(t, root)
			if _, err := RecoverState(t.Context(), root, source, true); err == nil {
				t.Error("completed retry reported success with an unavailable previous archive")
			}
			recoveryTestUnchanged(t, root, before)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func recoveryTestStage(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var stage string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "recovery-") {
			if stage != "" {
				t.Fatal("multiple recovery stages")
			}
			stage = filepath.Join(root, entry.Name())
		}
	}
	if stage == "" {
		t.Fatal("pending recovery did not retain its stage")
	}
	return stage
}

func TestRecoverStateResumeRejectsSubstitutedFilesAndDirectories(t *testing.T) {
	for _, kind := range []string{"stage directory", "previous directory", "staged replacement", "live old database", "archived old database", "installed replacement"} {
		t.Run(kind, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			old := recoveryTestRawBundle(t, root)
			boundary := "journal-published"
			if kind == "archived old database" {
				boundary = "archived-state.sqlite3"
			} else if kind == "installed replacement" {
				boundary = "replacement-installed"
			}
			failure := errors.New("pause for identity substitution")
			hit := recoveryTestFault(t, boundary, failure)
			if _, err := RecoverState(t.Context(), root, source, true); !*hit || !errors.Is(err, failure) {
				t.Fatalf("could not interrupt recovery: %v", err)
			}
			recoveryBoundary = func(string) error { return nil }
			stage := recoveryTestStage(t, root)
			var target string
			switch kind {
			case "stage directory":
				target = stage
			case "previous directory":
				target = filepath.Join(stage, "previous")
			case "staged replacement":
				target = filepath.Join(stage, "replacement.sqlite3")
			case "live old database", "installed replacement":
				target = filepath.Join(root, StateDatabaseFilename)
			case "archived old database":
				target = filepath.Join(stage, "previous", StateDatabaseFilename)
			}
			held := filepath.Join(recoveryTestDir(t), "original")
			if err := os.Rename(target, held); err != nil {
				t.Fatal(err)
			}
			isDirectory := strings.HasSuffix(kind, "directory")
			if isDirectory {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				// Keep every child inode and byte intact: only the directory
				// identity changes, so file manifests alone cannot catch this.
				recoveryTestMoveChildren(t, held, target)
			} else {
				data, err := os.ReadFile(held)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryTestSnapshot(t, root)
			result, err := RecoverState(t.Context(), root, source, true)
			if err == nil || result.Applied {
				t.Errorf("substituted recovery identity accepted: %+v, %v", result, err)
			}
			recoveryTestUnchanged(t, root, before)
			recoveryTestPending(t, root)
			if isDirectory {
				recoveryTestMoveChildren(t, target, held)
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(held, target); err != nil {
				t.Fatal(err)
			}
			result, err = RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed {
				t.Fatalf("restored identity could not resume: %+v, %v", result, err)
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func recoveryTestMoveChildren(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// These names are the fault-injection protocol, not the recovery algorithm.
var recoveryTestBoundaries = []string{
	"journal-published",
	"archive-renamed-state.sqlite3",
	"archived-state.sqlite3",
	"archive-renamed-state.sqlite3-wal",
	"archived-state.sqlite3-wal",
	"archive-renamed-state.sqlite3-shm",
	"archived-state.sqlite3-shm",
	"archive-renamed-state.sqlite3-journal",
	"archived-state.sqlite3-journal",
	"replacement-installed",
	"receipt-published",
	"before-journal-removal",
	"journal-removed",
}

func TestRecoverStateRetriesFailedArchiveAndStageSyncs(t *testing.T) {
	for _, phase := range []string{"archived directory", "installed replacement stage", "installed replacement root"} {
		t.Run(phase, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			// Only one old file: retry cannot accidentally satisfy the sync
			// requirement while archiving another file later in its loop.
			if err := os.WriteFile(filepath.Join(root, StateDatabaseFilename), []byte("old unreadable main database"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := recoveryTestDatabaseObjects(t, root)
			originalBoundary, originalSync := recoveryBoundary, recoverySyncDirectory
			t.Cleanup(func() { recoveryBoundary, recoverySyncDirectory = originalBoundary, originalSync })
			var stage string
			var target fs.FileInfo
			archived := false
			recoveryBoundary = func(point string) error {
				if point == "journal-published" {
					stage = recoveryTestStage(t, root)
					path := stage
					if phase == "archived directory" {
						path = filepath.Join(stage, "previous")
					} else if phase == "installed replacement root" {
						path = root
					}
					var err error
					target, err = os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
				}
				if point == "archive-renamed-state.sqlite3" {
					archived = true
				}
				return nil
			}
			failure := errors.New("injected directory sync failure")
			failures := 0
			failedThisAttempt := false
			recoverySyncDirectory = func(directory *os.File) error {
				if failedThisAttempt {
					t.Error("recovery synced another directory after a failed rename sync")
				}
				info, err := directory.Stat()
				if err != nil {
					return err
				}
				if target != nil && os.SameFile(info, target) && archived {
					_, replacementErr := os.Lstat(filepath.Join(stage, "replacement.sqlite3"))
					if phase == "archived directory" || errors.Is(replacementErr, os.ErrNotExist) {
						failures++
						failedThisAttempt = true
						return failure
					}
				}
				return directory.Sync()
			}
			for attempt := range 2 {
				failedThisAttempt = false
				result, err := RecoverState(t.Context(), root, source, true)
				if !errors.Is(err, failure) || result.Applied || failures != attempt+1 {
					t.Fatalf("attempt %d bypassed required sync: failures=%d result=%+v err=%v", attempt+1, failures, result, err)
				}
				recoveryTestAssertArchive(t, root, old)
				recoveryTestPending(t, root)
			}
			recoverySyncDirectory = originalSync
			result, err := RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed {
				t.Fatalf("retry after sync became available = %+v, %v", result, err)
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestRepeat(t, root, source, result)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func TestRecoverStateCompletedRetryConfirmsFailedFinalSync(t *testing.T) {
	source, want := recoveryTestSource(t)
	root := recoveryTestDir(t)
	old := recoveryTestRawBundle(t, root)
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	originalBoundary, originalSync := recoveryBoundary, recoverySyncDirectory
	t.Cleanup(func() { recoveryBoundary, recoverySyncDirectory = originalBoundary, originalSync })
	retiring := false
	recoveryBoundary = func(point string) error {
		if point == "before-journal-removal" {
			retiring = true
		}
		return nil
	}
	failure := errors.New("injected final root sync failure")
	failures := 0
	recoverySyncDirectory = func(directory *os.File) error {
		info, err := directory.Stat()
		if err != nil {
			return err
		}
		if retiring && os.SameFile(info, rootInfo) {
			failures++
			return failure
		}
		return directory.Sync()
	}
	first, err := RecoverState(t.Context(), root, source, true)
	if !errors.Is(err, failure) || !first.Applied || first.Resumed || failures != 1 {
		t.Fatalf("uncertain final sync = %+v, %v (failures=%d)", first, err, failures)
	}
	recoveryTestAssertArchive(t, root, old)
	// The marker is already gone. An ordinary read must succeed, but recovery
	// must still confirm durable retirement before reporting a successful retry.
	recoveryTestAssertState(t, root, want)
	before := recoveryTestSnapshot(t, root)
	result, err := RecoverState(t.Context(), root, source, true)
	if !errors.Is(err, failure) || !result.Applied || !result.Resumed || failures != 2 || result.Previous != first.Previous {
		t.Fatalf("completed receipt skipped final sync: %+v, %v (failures=%d)", result, err, failures)
	}
	recoveryTestUnchanged(t, root, before)
	recoverySyncDirectory = originalSync
	recoveryTestRepeat(t, root, source, first)
	recoveryTestAssertArchive(t, root, old)
}

func TestRecoverStateResurrectedMarkerPreservesAdvancedDatabaseAndWAL(t *testing.T) {
	for _, advancement := range []string{"leave-committed-wal", "leave-advanced-main-and-wal"} {
		t.Run(advancement, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			old := recoveryTestRawBundle(t, root)
			first, err := RecoverState(t.Context(), root, source, true)
			if err != nil {
				t.Fatal(err)
			}
			originalMain := recoveryTestDatabaseObjects(t, root)[StateDatabaseFilename]
			marker, err := os.ReadFile(filepath.Join(root, "state-recovery-completed.json"))
			if err != nil {
				t.Fatal(err)
			}
			recoveryTestRunChild(t, root, source, advancement)
			want.registry.Preferences.DesktopIndicators = true
			if advancement == "leave-advanced-main-and-wal" {
				want.registry.Contexts[0].Label += "-advanced"
			}
			advanced := recoveryTestDatabaseObjects(t, root)
			if len(advanced[StateDatabaseFilename+"-wal"].Data) <= 32 || len(advanced[StateDatabaseFilename+"-shm"].Data) == 0 {
				t.Fatal("advancement fixture did not leave WAL and SHM")
			}
			if advancement == "leave-advanced-main-and-wal" && advanced[StateDatabaseFilename].Data == originalMain.Data {
				t.Fatal("advancement fixture did not checkpoint a change to the main database")
			}
			// Simulate power loss resurrecting an unsynced marker unlink.
			// The durable receipt describes the same completed operation.
			if err := os.WriteFile(filepath.Join(root, "state-recovery.json"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
			recoveryTestPending(t, root)
			before := recoveryTestSnapshot(t, root)
			result, err := RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed || result.Previous != first.Previous || result.PreviousValid != first.PreviousValid {
				t.Fatalf("resurrected marker did not retire: %+v, %v", result, err)
			}
			if !reflect.DeepEqual(recoveryTestDatabaseObjects(t, root), advanced) {
				t.Error("retiring a completed marker changed the advanced main/WAL/SHM")
			}
			after := recoveryTestSnapshot(t, root)
			if _, exists := after["state-recovery.json"]; exists {
				t.Error("completed marker still exists")
			}
			// Only the marker and its parent-directory mtime may change.
			delete(before, "state-recovery.json")
			delete(before, ".")
			delete(after, ".")
			if !reflect.DeepEqual(before, after) {
				t.Error("retiring a completed marker changed files or created another archive")
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func TestRecoverStateRejectsMalformedRecoveryRecordsWithoutEffects(t *testing.T) {
	for _, recordName := range []string{"state-recovery.json", "state-recovery-completed.json"} {
		for _, kind := range []string{"truncated", "empty object", "newer version", "unknown field", "trailing document", "oversized", "unsafe previous name"} {
			t.Run(recordName+"/"+kind, func(t *testing.T) {
				source, want := recoveryTestSource(t)
				root := recoveryTestDir(t)
				old := recoveryTestRawBundle(t, root)
				pending := recordName == "state-recovery.json"
				if pending {
					failure := errors.New("stop with a published journal")
					hit := recoveryTestFault(t, "journal-published", failure)
					if _, err := RecoverState(t.Context(), root, source, true); !*hit || !errors.Is(err, failure) {
						t.Fatalf("could not prepare pending recovery: %v", err)
					}
					recoveryBoundary = func(string) error { return nil }
				} else if _, err := RecoverState(t.Context(), root, source, true); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, recordName)
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]json.RawMessage
				if err := json.Unmarshal(original, &record); err != nil {
					t.Fatal(err)
				}
				var bad []byte
				switch kind {
				case "truncated":
					bad = original[:len(original)/2]
				case "empty object":
					bad = []byte("{}")
				case "trailing document":
					bad = append(append([]byte(nil), original...), []byte("\n{}")...)
				case "oversized":
					bad = append(bytes.Repeat([]byte(" "), 100*1024), original...)
				case "newer version":
					record["version"] = json.RawMessage("999")
				case "unknown field":
					record["unrecognized"] = json.RawMessage("true")
				case "unsafe previous name":
					var previous []map[string]json.RawMessage
					if err := json.Unmarshal(record["previous"], &previous); err != nil || len(previous) == 0 {
						t.Fatalf("no previous files in fixture: %v", err)
					}
					previous[0]["name"] = json.RawMessage(`"../outside.sqlite3"`)
					record["previous"], err = json.Marshal(previous)
					if err != nil {
						t.Fatal(err)
					}
				}
				if bad == nil {
					bad, err = json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, bad, 0o600); err != nil {
					t.Fatal(err)
				}
				before := recoveryTestSnapshot(t, root)
				if _, err := RecoverState(t.Context(), root, source, true); err == nil {
					t.Error("malformed recovery record accepted")
				}
				recoveryTestUnchanged(t, root, before)
				if pending {
					recoveryTestPending(t, root)
				}
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				if result, err := RecoverState(t.Context(), root, source, true); err != nil || !result.Applied || !result.Resumed {
					t.Fatalf("restored record could not resume: %+v, %v", result, err)
				}
				recoveryTestAssertArchive(t, root, old)
				recoveryTestAssertState(t, root, want)
			})
		}
	}
}

func TestRecoverStatePublicationSyncFailureRetainsRecoveryDependencies(t *testing.T) {
	for _, phase := range []string{"journal", "journal with inaccessible root", "receipt"} {
		t.Run(phase, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			old := recoveryTestRawBundle(t, root)
			rootInfo, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			originalSync := recoverySyncDirectory
			t.Cleanup(func() {
				recoverySyncDirectory = originalSync
				if err := os.Chmod(root, 0o700); err != nil {
					t.Error(err)
				}
			})
			failure := errors.New("injected publication sync failure")
			failed := false
			recoverySyncDirectory = func(directory *os.File) error {
				info, err := directory.Stat()
				if err != nil {
					return err
				}
				name := "state-recovery.json"
				if phase == "receipt" {
					name = "state-recovery-completed.json"
				}
				if os.SameFile(info, rootInfo) {
					if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
						failed = true
						if phase == "journal with inaccessible root" {
							// A follow-up pathname inspection may itself fail.
							// Publication must still retain all referenced files.
							if err := os.Chmod(root, 0o600); err != nil {
								t.Fatal(err)
							}
						}
						return failure
					}
				}
				return directory.Sync()
			}
			result, err := RecoverState(t.Context(), root, source, true)
			if chmodErr := os.Chmod(root, 0o700); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			if !failed || !errors.Is(err, failure) || result.Applied {
				t.Fatalf("publication failure = %+v, %v (injected=%v)", result, err, failed)
			}
			recoveryTestPending(t, root)
			if phase == "receipt" {
				recoveryTestAssertArchive(t, root, old)
			} else {
				if !reflect.DeepEqual(recoveryTestDatabaseObjects(t, root), old) {
					t.Error("journal publication failure changed old live files")
				}
				stage := recoveryTestStage(t, root)
				if _, err := ValidateStateBackup(t.Context(), filepath.Join(stage, "replacement.sqlite3")); err != nil {
					t.Fatalf("published journal lost replacement dependency: %v", err)
				}
			}
			recoverySyncDirectory = originalSync
			result, err = RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed {
				t.Fatalf("publication failure could not resume: %+v, %v", result, err)
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func recoveryTestFault(t *testing.T, boundary string, failure error) *bool {
	t.Helper()
	old := recoveryBoundary
	hit := false
	recoveryBoundary = func(point string) error {
		if point == boundary {
			hit = true
			return failure
		}
		return nil
	}
	t.Cleanup(func() { recoveryBoundary = old })
	return &hit
}

func TestRecoverStateResumesAfterBoundaryErrors(t *testing.T) {
	for _, boundary := range recoveryTestBoundaries {
		t.Run(boundary, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			old := recoveryTestRawBundle(t, root)
			failure := errors.New("injected recovery I/O failure")
			hit := recoveryTestFault(t, boundary, failure)
			result, err := RecoverState(t.Context(), root, source, true)
			if !*hit || !errors.Is(err, failure) || result.Applied != (boundary == "journal-removed") {
				t.Fatalf("boundary failure was lost: hit=%v result=%+v err=%v", *hit, result, err)
			}
			if boundary != "journal-removed" {
				recoveryTestPending(t, root)
			} else {
				// Marker removal already committed recovery even if the
				// caller received an error instead of its final response.
				recoveryTestAssertState(t, root, want)
			}
			recoveryBoundary = func(string) error { return nil }
			result, err = RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed || result.Previous == "" || result.PreviousValid {
				t.Fatalf("resume failed: %+v, %v", result, err)
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestRepeat(t, root, source, result)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func TestRecoverStateResumesAfterProcessDeath(t *testing.T) {
	for _, boundary := range recoveryTestBoundaries {
		t.Run(boundary, func(t *testing.T) {
			source, want := recoveryTestSource(t)
			root := recoveryTestDir(t)
			old := recoveryTestRawBundle(t, root)
			recoveryTestRunChild(t, root, source, boundary)
			if boundary != "journal-removed" {
				recoveryTestPending(t, root)
			} else {
				// A process can die after removing the marker but before
				// returning success. Ordinary reads must already work.
				recoveryTestAssertState(t, root, want)
			}
			beforeResume := recoveryTestSnapshot(t, root)
			result, err := RecoverState(t.Context(), root, source, true)
			if err != nil || !result.Applied || !result.Resumed || result.Previous == "" || result.PreviousValid {
				t.Fatalf("resume after death failed: %+v, %v", result, err)
			}
			if boundary == "journal-removed" {
				recoveryTestUnchanged(t, root, beforeResume)
			}
			recoveryTestAssertArchive(t, root, old)
			recoveryTestRepeat(t, root, source, result)
			recoveryTestAssertState(t, root, want)
		})
	}
}

func recoveryTestRunChild(t *testing.T, root, source, boundary string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecoverStateProcessHelper$", "-test.count=1")
	// Do not inherit live Sway, Herdr, XDG, or session environment values.
	cmd.Env = []string{
		"SWAY_SESSION_RECOVERY_TEST_ROOT=" + root,
		"SWAY_SESSION_RECOVERY_TEST_SOURCE=" + source,
		"SWAY_SESSION_RECOVERY_TEST_BOUNDARY=" + boundary,
		"XDG_STATE_HOME=" + recoveryTestDir(t),
		"XDG_RUNTIME_DIR=" + recoveryTestDir(t),
		"XDG_CONFIG_HOME=" + recoveryTestDir(t),
	}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("child did not die at %s with exit 77: %v\n%s", boundary, err, output)
	}
}

func TestRecoverStateProcessHelper(t *testing.T) {
	boundary := os.Getenv("SWAY_SESSION_RECOVERY_TEST_BOUNDARY")
	if boundary == "" {
		return
	}
	root, source := os.Getenv("SWAY_SESSION_RECOVERY_TEST_ROOT"), os.Getenv("SWAY_SESSION_RECOVERY_TEST_SOURCE")
	if root == "" || !filepath.IsAbs(root) || source == "" || !filepath.IsAbs(source) || os.Getenv("XDG_STATE_HOME") == "" {
		t.Fatal("recovery subprocess requires explicit disposable paths")
	}
	if boundary == "leave-committed-wal" || boundary == "leave-advanced-main-and-wal" {
		// Keep a real SQLite connection alive through os.Exit. Normal Close
		// would checkpoint/delete sidecars and hide precisely the crash state
		// whose preservation recovery promises.
		database, err := openStateDatabase(t.Context(), root, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
			t.Fatal(err)
		}
		var registry Registry
		if err := RegistryStoreFor(root).LoadInto(&registry); err != nil {
			t.Fatal(err)
		}
		if boundary == "leave-advanced-main-and-wal" {
			registry.Contexts[0].Label += "-advanced"
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
		}
		registry.Preferences.DesktopIndicators = true
		if err := RegistryStoreFor(root).Save(registry); err != nil {
			t.Fatal(err)
		}
		os.Exit(77)
	}
	recoveryBoundary = func(point string) error {
		if point == boundary {
			os.Exit(77)
		}
		return nil
	}
	result, err := RecoverState(t.Context(), root, source, true)
	t.Fatalf("recovery returned without dying at %s: %+v, %v", boundary, result, err)
}
