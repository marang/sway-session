package statefile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type testDocument struct {
	Version int    `json:"version"`
	Value   string `json:"value"`
}

func validateTestDocument(document *testDocument) error {
	if document.Version != 1 {
		return errors.New("unsupported version")
	}
	if document.Value == "" {
		return errors.New("value is required")
	}
	return nil
}

func TestSaveAndLoadUsePrivateAtomicState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	want := testDocument{Version: 1, Value: "last valid"}

	if err := file.Save(want); err != nil {
		t.Fatalf("save state: %v", err)
	}
	assertMode(t, root, DirectoryMode)
	assertMode(t, filepath.Join(root, "state.json"), RegularFileMode)
	temporary, err := filepath.Glob(filepath.Join(root, ".state.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temporary files: %v", err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary files remain after atomic write: %v", temporary)
	}

	var got testDocument
	if err := file.LoadInto(&got); err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got != want {
		t.Fatalf("unexpected state: got=%+v want=%+v", got, want)
	}
}

func TestLoadSnapshotDoesNotWaitBehindExternalTransactionLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	want := testDocument{Version: 1, Value: "available during transaction"}
	if err := file.Save(want); err != nil {
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

	result := make(chan error, 1)
	go func() {
		var got testDocument
		err := file.LoadSnapshotInto(&got)
		if err == nil && got != want {
			err = fmt.Errorf("snapshot mismatch: got %+v want %+v", got, want)
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot read waited behind the transaction lock")
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

func TestUpdateReportsVisibleCommitWhenDirectorySyncFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	initial := testDocument{Version: 1, Value: "initial"}
	if err := file.Save(initial); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	wantSyncError := errors.New("directory sync failed")
	file.syncAfterRename = func(*os.File) error {
		return wantSyncError
	}
	updated, err := file.Update(initial, func(document *testDocument) error {
		document.Value = "updated"
		return nil
	})
	var unknown *CommitOutcomeUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, wantSyncError) {
		t.Fatalf("expected unknown commit outcome wrapping sync failure, got %v", err)
	}
	if updated.Value != "updated" {
		t.Fatalf("update returned stale state after visible commit: %+v", updated)
	}

	var visible testDocument
	if err := file.LoadInto(&visible); err != nil {
		t.Fatalf("load visible state after sync failure: %v", err)
	}
	if visible != updated {
		t.Fatalf("visible state does not match returned candidate: got=%+v want=%+v", visible, updated)
	}
}

func TestInvalidSavePreservesLastValidFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	if err := file.Save(testDocument{Version: 1, Value: "last valid"}); err != nil {
		t.Fatalf("save initial state: %v", err)
	}
	path := filepath.Join(root, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}

	if err := file.Save(testDocument{Version: 2, Value: "invalid"}); err == nil {
		t.Fatal("expected invalid candidate to be rejected")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after rejected save: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("last valid file changed after rejected save:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestLoadCorruptionPreservesLastKnownGoodTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	if err := file.Save(testDocument{Version: 1, Value: "persisted"}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"value":`), RegularFileMode); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}

	current := testDocument{Version: 1, Value: "in memory"}
	if err := file.LoadInto(&current); err == nil {
		t.Fatal("expected malformed state to be rejected")
	}
	if current != (testDocument{Version: 1, Value: "in memory"}) {
		t.Fatalf("last known-good target changed: %+v", current)
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	for _, contents := range []string{
		`{"version":1,"value":"ok","command":"sh"}`,
		`{"version":1,"value":"ok"} {"version":1,"value":"second"}`,
	} {
		t.Run(contents, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, DirectoryMode); err != nil {
				t.Fatalf("create state directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(contents), RegularFileMode); err != nil {
				t.Fatalf("write state: %v", err)
			}
			file := NewJSONFile(root, "state.json", validateTestDocument)
			var target testDocument
			if err := file.LoadInto(&target); err == nil {
				t.Fatal("expected strict JSON decoder to reject state")
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
		file := NewJSONFile(linkedRoot, "state.json", validateTestDocument)
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
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
		file := NewJSONFile(filepath.Join(linkedParent, "state"), "state.json", validateTestDocument)
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
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
		file := NewJSONFile(root, "state.json", validateTestDocument)
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
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

	file := NewJSONFile(root, "state.json", validateTestDocument)
	data, err := file.encode(testDocument{Version: 1, Value: "safe"})
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	if err := file.saveAt(directory, data); err != nil {
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
		file := NewJSONFile(root, "state.json", validateTestDocument)
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
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
		file := NewJSONFile(root, "state.json", validateTestDocument)
		var target testDocument
		if err := file.LoadInto(&target); err == nil {
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
		file := NewJSONFile(root, "state.json", validateTestDocument)
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
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
		file := NewJSONFile(root, "state.json", validateTestDocument)
		finished := make(chan error, 1)
		go func() {
			var target testDocument
			finished <- file.LoadInto(&target)
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

func TestUpdateSerializesTwoProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	if err := file.Save(testDocument{Version: 1, Value: "initial"}); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	coordination := t.TempDir()
	firstEntered := filepath.Join(coordination, "first-entered")
	firstRelease := filepath.Join(coordination, "first-release")
	secondReady := filepath.Join(coordination, "second-ready")
	secondEntered := filepath.Join(coordination, "second-entered")

	first := startUpdateProcess(t, root, "A", "", firstEntered, firstRelease)
	waitForFile(t, firstEntered, 5*time.Second)
	second := startUpdateProcess(t, root, "B", secondReady, secondEntered, "")
	waitForFile(t, secondReady, 5*time.Second)

	secondEnteredBeforeRelease := fileAppears(secondEntered, 500*time.Millisecond)
	if err := os.WriteFile(firstRelease, nil, RegularFileMode); err != nil {
		t.Fatalf("release first update: %v", err)
	}
	waitUpdateProcess(t, first)
	waitUpdateProcess(t, second)
	if secondEnteredBeforeRelease {
		t.Fatal("second process entered its mutation while the first update still held the state lock")
	}

	var got testDocument
	if err := file.LoadInto(&got); err != nil {
		t.Fatalf("load updated state: %v", err)
	}
	if got.Value != "initialAB" {
		t.Fatalf("concurrent updates lost a change: got %q want %q", got.Value, "initialAB")
	}
}

func TestStatefileUpdateProcessHelper(t *testing.T) {
	if os.Getenv("STATEFILE_UPDATE_HELPER") != "1" {
		return
	}
	root := os.Getenv("STATEFILE_UPDATE_ROOT")
	token := os.Getenv("STATEFILE_UPDATE_TOKEN")
	ready := os.Getenv("STATEFILE_UPDATE_READY")
	entered := os.Getenv("STATEFILE_UPDATE_ENTERED")
	release := os.Getenv("STATEFILE_UPDATE_RELEASE")
	if ready != "" {
		if err := os.WriteFile(ready, nil, RegularFileMode); err != nil {
			t.Fatalf("signal helper readiness: %v", err)
		}
	}

	file := NewJSONFile(root, "state.json", validateTestDocument)
	_, err := file.Update(testDocument{Version: 1, Value: "initial"}, func(document *testDocument) error {
		document.Value += token
		if err := os.WriteFile(entered, nil, RegularFileMode); err != nil {
			return err
		}
		if release != "" && !fileAppears(release, 10*time.Second) {
			return errors.New("timed out waiting for update release")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("update state: %v", err)
	}
}

func TestInspectLockedSerializesExternalObservationWithUpdate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	file := NewJSONFile(root, "state.json", validateTestDocument)
	initial := testDocument{Version: 1, Value: "before"}
	if err := file.Save(initial); err != nil {
		t.Fatal(err)
	}
	inspectionStarted := make(chan struct{})
	releaseInspection := make(chan struct{})
	inspectionDone := make(chan error, 1)
	go func() {
		inspectionDone <- file.InspectLocked(testDocument{}, func(observed testDocument) error {
			if observed != initial {
				return fmt.Errorf("unexpected observed state: %+v", observed)
			}
			close(inspectionStarted)
			<-releaseInspection
			return nil
		})
	}()
	<-inspectionStarted

	mutationEntered := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		_, err := file.Update(initial, func(candidate *testDocument) error {
			close(mutationEntered)
			candidate.Value = "after"
			return nil
		})
		updateDone <- err
	}()
	select {
	case <-mutationEntered:
		t.Fatal("update entered while inspection held the directory lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseInspection)
	if err := <-inspectionDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-mutationEntered:
	case <-time.After(time.Second):
		t.Fatal("update did not acquire lock after inspection completed")
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
}

type updateProcess struct {
	command *exec.Cmd
	output  bytes.Buffer
}

func startUpdateProcess(t *testing.T, root string, token string, ready string, entered string, release string) *updateProcess {
	t.Helper()
	process := &updateProcess{command: exec.Command(os.Args[0], "-test.run=^TestStatefileUpdateProcessHelper$")}
	process.command.Env = append(os.Environ(),
		"STATEFILE_UPDATE_HELPER=1",
		"STATEFILE_UPDATE_ROOT="+root,
		"STATEFILE_UPDATE_TOKEN="+token,
		"STATEFILE_UPDATE_READY="+ready,
		"STATEFILE_UPDATE_ENTERED="+entered,
		"STATEFILE_UPDATE_RELEASE="+release,
	)
	process.command.Stdout = &process.output
	process.command.Stderr = &process.output
	if err := process.command.Start(); err != nil {
		t.Fatalf("start update helper: %v", err)
	}
	t.Cleanup(func() {
		if process.command.ProcessState == nil {
			_ = process.command.Process.Kill()
			_ = process.command.Wait()
		}
	})
	return process
}

func waitUpdateProcess(t *testing.T, process *updateProcess) {
	t.Helper()
	if err := process.command.Wait(); err != nil {
		t.Fatalf("update helper failed: %v\n%s", err, process.output.String())
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	if !fileAppears(path, timeout) {
		t.Fatalf("timed out waiting for %s", path)
	}
}

func fileAppears(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestStateFileRejectsUnsafePaths(t *testing.T) {
	for _, file := range []JSONFile[testDocument]{
		NewJSONFile("relative/state", "state.json", validateTestDocument),
		NewJSONFile(t.TempDir()+string(os.PathSeparator)+"state"+string(os.PathSeparator)+".."+string(os.PathSeparator)+"other", "state.json", validateTestDocument),
		NewJSONFile(filepath.Join(t.TempDir(), "state"), "../state.json", validateTestDocument),
	} {
		if err := file.Save(testDocument{Version: 1, Value: "unsafe"}); err == nil {
			t.Fatal("expected unsafe state path to be rejected")
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
