package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marang/sway-session/internal/diagnostic"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/statefile"
	"golang.org/x/sys/unix"
)

func TestStateCommandArgumentsAndJSON(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		apply bool
	}{
		{"backup", []string{"backup", "--output", "/private/backup file.sqlite3"}, false},
		{"backup equals", []string{"backup", "--output=/private/backup file.sqlite3"}, false},
		{"preview", []string{"recover", "--from", "/private/backup file.sqlite3"}, false},
		{"explicit preview", []string{"recover", "--from=/private/backup file.sqlite3", "--yes=false"}, false},
		{"apply", []string{"recover", "--from", "/private/backup file.sqlite3", "--yes"}, true},
		{"apply before source", []string{"recover", "--yes", "--from=/private/backup file.sqlite3"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", runtime)
			root := filepath.Join(t.TempDir(), "state")
			called := false
			check := func(gotRoot, path string) {
				t.Helper()
				called = true
				if gotRoot != root || path != "/private/backup file.sqlite3" {
					t.Fatalf("root=%q path=%q", gotRoot, path)
				}
			}
			deps := dependencies{
				stateRoot: func() (string, error) { return root, nil },
				backupState: func(_ context.Context, gotRoot, path string) (sessionstate.StateBackupResult, error) {
					check(gotRoot, path)
					return sessionstate.StateBackupResult{Path: path, SizeBytes: 4096, SchemaVersion: 1, ContextCount: 2}, nil
				},
				recoverState: func(_ context.Context, gotRoot, path string, apply bool) (sessionstate.StateRecoveryResult, error) {
					check(gotRoot, path)
					if apply != test.apply {
						t.Fatalf("apply=%v want %v", apply, test.apply)
					}
					return sessionstate.StateRecoveryResult{Source: path, Database: filepath.Join(root, "state.sqlite3"), Applied: apply}, nil
				},
			}
			var out, errout bytes.Buffer
			args := append([]string{"--json", "state"}, test.args...)
			code := runWith(args, strings.NewReader(""), &out, &errout, deps)
			var result commandResult
			if code != exitSuccess || errout.Len() != 0 || !called || json.Unmarshal(out.Bytes(), &result) != nil {
				t.Fatalf("code=%d called=%v stdout=%s stderr=%s", code, called, &out, &errout)
			}
			if result.Version != commandResultVersion || result.Command != "state "+test.args[0] || len(result.Contexts) != 0 {
				t.Fatalf("unexpected envelope: %+v", result)
			}
			if test.args[0] == "backup" {
				if result.StateBackup == nil || result.StateRecovery != nil || result.Preview || !strings.Contains(out.String(), `"state_backup":`) {
					t.Fatalf("backup envelope: %s", &out)
				}
				if result.StateBackup.Path != "/private/backup file.sqlite3" || result.StateBackup.SizeBytes != 4096 || result.StateBackup.SchemaVersion != 1 || result.StateBackup.ContextCount != 2 {
					t.Fatalf("backup summary: %+v", result.StateBackup)
				}
			} else if result.StateRecovery == nil || result.StateBackup != nil || result.Preview == test.apply || result.StateRecovery.Applied != test.apply || !strings.Contains(out.String(), `"state_recovery":`) {
				t.Fatalf("recovery envelope: %s", &out)
			}
			if !test.apply {
				entries, err := os.ReadDir(runtime)
				if err != nil || len(entries) != 0 {
					t.Fatalf("non-apply command changed runtime: %v %v", entries, err)
				}
			}
		})
	}
}

func TestStateInvalidArgumentsHaveNoDependencies(t *testing.T) {
	for _, args := range [][]string{
		{}, {"unknown"}, {"backup"}, {"recover"},
		{"backup", "--output"}, {"recover", "--from"},
		{"backup", "--output="}, {"recover", "--from="},
		{"backup", "--output", "relative"}, {"recover", "--from", "/private/../backup"},
		{"backup", "--output", "/"}, {"recover", "--from", "/private/backup/"},
		{"backup", "--output", "/backup", "extra"}, {"recover", "--from", "/backup", "extra"},
		{"backup", "--from", "/backup"}, {"recover", "--output", "/backup"},
		{"backup", "--output", "/backup", "--yes"}, {"recover", "--from", "/backup", "--force"},
		{"backup", "--output", "/backup", "--output", "/other"}, {"recover", "--from", "/backup", "--from", "/other"},
		{"recover", "--from", "/backup", "--yes=invalid"}, {"recover", "--from", "/backup", "--yes", "true"},
		{"recover", "--from", "/backup", "--config", "/config"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errout bytes.Buffer
			code := runWith(append([]string{"--json", "state"}, args...), strings.NewReader(""), &out, &errout, dependencies{})
			if code != exitUsage || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errout)
			}
			assertStateDiagnostic(t, errout.Bytes(), "usage")
		})
	}
}

func TestStateListReportsRecoveryAccessRefusal(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "exclusive gate"
		if pending {
			name = "pending recovery"
		}
		t.Run(name, func(t *testing.T) {
			setSessionTestStateHome(t, t.TempDir())
			deps := defaultDependencies(strings.NewReader(""))
			deps.workingDir = func() (string, error) { return t.TempDir(), nil }
			storeRestoreTestContexts(t, deps, 1)
			root, err := deps.stateRoot()
			if err != nil {
				t.Fatal(err)
			}
			code := "state_database_busy"
			var clearRefusal func()
			if pending {
				// Ordinary access must refuse even a malformed marker without
				// parsing, repairing, or disclosing its private contents.
				marker := filepath.Join(root, "state-recovery.json")
				if err := os.WriteFile(marker, []byte("{PRIVATE RECOVERY PAYLOAD"), 0o600); err != nil {
					t.Fatal(err)
				}
				code = "state_recovery_pending"
				clearRefusal = func() {
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				gate, err := os.OpenFile(filepath.Join(root, ".state-access.lock"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Close()
				if err := unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				clearRefusal = func() {
					if err := unix.Flock(int(gate.Fd()), unix.LOCK_UN); err != nil {
						t.Fatal(err)
					}
				}
			}
			var out, errout bytes.Buffer
			if status := runWith([]string{"--json", "list"}, strings.NewReader(""), &out, &errout, deps); status != exitOperation || out.Len() != 0 {
				t.Fatalf("status=%d stdout=%s stderr=%s", status, &out, &errout)
			}
			assertStateDiagnostic(t, errout.Bytes(), code)
			if pending {
				if !strings.Contains(errout.String(), "same source") || !strings.Contains(errout.String(), "state recover --from PATH --yes") {
					t.Fatalf("pending recovery omitted same-source resume guidance: %s", &errout)
				}
			} else if !strings.Contains(errout.String(), "retry") {
				t.Fatalf("busy state omitted retry guidance: %s", &errout)
			}
			if strings.Contains(errout.String(), "PRIVATE RECOVERY PAYLOAD") || strings.Contains(errout.String(), "Restore 1") {
				t.Fatalf("refusal disclosed private state: %s", &errout)
			}
			// Removing only the fixture's refusal makes the same command work;
			// the failed read must not have damaged or replaced its registry.
			clearRefusal()
			out.Reset()
			errout.Reset()
			if status := runWith([]string{"--json", "list"}, strings.NewReader(""), &out, &errout, deps); status != exitSuccess || errout.Len() != 0 {
				t.Fatalf("list after fixture release: status=%d stdout=%s stderr=%s", status, &out, &errout)
			}
			var result commandResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Contexts) != 1 {
				t.Fatalf("registry changed on refusal: %s (%v)", &out, err)
			}
		})
	}
}

func TestStateRecoveryPreviewDoesNotPrepareStateOrRuntime(t *testing.T) {
	for _, runtimeExists := range []bool{false, true} {
		t.Run(fmt.Sprint(runtimeExists), func(t *testing.T) {
			base := t.TempDir()
			runtime := filepath.Join(base, "runtime")
			root := filepath.Join(base, "state")
			source := filepath.Join(base, "backup.sqlite3")
			payload := []byte("private fixture; the injected operation owns validation")
			if err := os.WriteFile(source, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if runtimeExists {
				if err := os.Mkdir(runtime, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_RUNTIME_DIR", runtime)
			deps := dependencies{
				stateRoot: func() (string, error) { return root, nil },
				recoverState: func(_ context.Context, gotRoot, gotSource string, apply bool) (sessionstate.StateRecoveryResult, error) {
					if gotRoot != root || gotSource != source || apply {
						t.Fatalf("unexpected recovery call: %q %q %v", gotRoot, gotSource, apply)
					}
					return sessionstate.StateRecoveryResult{Source: source, Database: filepath.Join(root, "state.sqlite3")}, nil
				},
			}
			var out, errout bytes.Buffer
			if code := runWith([]string{"state", "recover", "--from", source}, strings.NewReader("yes\n"), &out, &errout, deps); code != exitSuccess {
				t.Fatalf("code=%d: %s", code, &errout)
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preview created state root: %v", err)
			}
			if runtimeExists {
				entries, err := os.ReadDir(runtime)
				if err != nil || len(entries) != 0 {
					t.Fatalf("preview created runtime entries: %v %v", entries, err)
				}
			} else if _, err := os.Stat(runtime); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preview created runtime root: %v", err)
			}
			if after, err := os.ReadFile(source); err != nil || !bytes.Equal(after, payload) {
				t.Fatalf("preview changed source: %v", err)
			}
			for _, want := range []string{"Recovery preview", "No state was replaced", "no rollback file or daemon lock", "does not prove exclusive access", "--yes"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("preview omitted %q: %s", want, &out)
				}
			}
			if strings.Contains(out.String(), string(payload)) {
				t.Fatal("preview disclosed the session payload")
			}
		})
	}
}

func TestStateRecoveryApplyRefusesDaemonAndInvalidRuntime(t *testing.T) {
	for _, mode := range []string{"running", "unset", "relative", "unclean", "file"} {
		t.Run(mode, func(t *testing.T) {
			runtime := t.TempDir()
			switch mode {
			case "unset":
				runtime = ""
			case "relative":
				runtime = "relative"
			case "unclean":
				runtime += "/../runtime"
			case "file":
				runtime = filepath.Join(runtime, "file")
				if err := os.WriteFile(runtime, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_RUNTIME_DIR", runtime)
			if mode == "running" {
				lock, err := acquireSessionDaemonLock()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lock.Close() }()
			}
			deps := dependencies{
				stateRoot: func() (string, error) { return "/unused/state", nil },
				recoverState: func(context.Context, string, string, bool) (sessionstate.StateRecoveryResult, error) {
					t.Fatal("recovery called without daemon exclusion")
					return sessionstate.StateRecoveryResult{}, nil
				},
			}
			var out, errout bytes.Buffer
			code := runWith([]string{"--json", "state", "recover", "--from", "/unused/backup", "--yes"}, strings.NewReader(""), &out, &errout, deps)
			if code != exitOperation || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, &out, &errout)
			}
			assertStateDiagnostic(t, errout.Bytes(), "state_recovery")
			if mode == "running" && !strings.Contains(errout.String(), "Stop the daemon yourself") {
				t.Fatalf("missing actionable refusal: %s", &errout)
			}
		})
	}
}

func TestStateRecoveryHoldsAndReleasesDaemonLock(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			called := false
			deps := dependencies{
				stateRoot: func() (string, error) { return "/unused/state", nil },
				recoverState: func(context.Context, string, string, bool) (sessionstate.StateRecoveryResult, error) {
					called = true
					lock, err := acquireSessionDaemonLock()
					if lock != nil {
						_ = lock.Close()
					}
					if !errors.Is(err, errSessionDaemonRunning) {
						t.Fatalf("daemon lock not held during recovery: %v", err)
					}
					if fail {
						return sessionstate.StateRecoveryResult{}, errors.New("fixture failure")
					}
					return sessionstate.StateRecoveryResult{Applied: true}, nil
				},
			}
			_, problem := executeState(t.Context(), []string{"recover", "--from", "/unused/backup", "--yes"}, deps)
			if !called || (problem != nil) != fail {
				t.Fatalf("called=%v problem=%+v", called, problem)
			}
			lock, err := acquireSessionDaemonLock()
			if err != nil {
				t.Fatalf("daemon lock leaked: %v", err)
			}
			_ = lock.Close()
		})
	}
}

func TestStateCancellationAndOperationalJSON(t *testing.T) {
	for _, subcommand := range []string{"backup", "recover"} {
		for _, test := range []struct {
			err  error
			code string
		}{
			{context.Canceled, "state_" + map[string]string{"backup": "backup", "recover": "recovery"}[subcommand]},
			{fmt.Errorf("wrapped: %w", sessionstate.ErrStateDatabaseBusy), "state_database_busy"},
			{fmt.Errorf("wrapped: %w", sessionstate.ErrStateRecoveryPending), "state_recovery_pending"},
		} {
			t.Run(subcommand+"/"+test.code, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				t.Setenv("XDG_RUNTIME_DIR", "")
				called := false
				check := func(got context.Context) {
					called = true
					if got != ctx || got.Err() != context.Canceled {
						t.Fatal("caller cancellation was not forwarded")
					}
				}
				deps := dependencies{
					stateRoot: func() (string, error) { return "/unused/state", nil },
					backupState: func(got context.Context, _, _ string) (sessionstate.StateBackupResult, error) {
						check(got)
						return sessionstate.StateBackupResult{}, test.err
					},
					recoverState: func(got context.Context, _, _ string, _ bool) (sessionstate.StateRecoveryResult, error) {
						check(got)
						return sessionstate.StateRecoveryResult{}, test.err
					},
				}
				option := "--from"
				if subcommand == "backup" {
					option = "--output"
				}
				var out, errout bytes.Buffer
				code := runWithContext(ctx, []string{"--json", "state", subcommand, option, "/unused/backup"}, strings.NewReader(""), &out, &errout, deps)
				if code != exitOperation || out.Len() != 0 || !called {
					t.Fatalf("code=%d called=%v stdout=%s stderr=%s", code, called, &out, &errout)
				}
				assertStateDiagnostic(t, errout.Bytes(), test.code)
			})
		}
	}
}

func TestStateRecoveryHumanRollbackLocations(t *testing.T) {
	for _, valid := range []bool{false, true} {
		var out bytes.Buffer
		result := commandResult{StateRecovery: &sessionstate.StateRecoveryResult{Source: "/fixture/backup", Database: "/fixture/state.sqlite3", Applied: true, Previous: "/fixture/previous", PreviousValid: valid, Resumed: true}}
		if err := writeResult(&out, false, result); err != nil {
			t.Fatal(err)
		}
		want := "raw previous-state bundle; not a validated backup"
		if valid {
			want = "validated previous database"
		}
		for _, phrase := range []string{"Resumed recovery", `Rollback location: "/fixture/previous"`, want, "Herdr history, application state, and configuration were not restored"} {
			if !strings.Contains(out.String(), phrase) {
				t.Fatalf("missing %q: %s", phrase, &out)
			}
		}
	}
}

func TestStatePartialFailurePreservesArtifactLocations(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, subcommand := range []string{"backup", "recover"} {
			t.Run(fmt.Sprintf("%s/json=%v", subcommand, structured), func(t *testing.T) {
				t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
				deps := dependencies{
					stateRoot: func() (string, error) { return "/fixture/state", nil },
					backupState: func(context.Context, string, string) (sessionstate.StateBackupResult, error) {
						return sessionstate.StateBackupResult{Path: "/fixture/backup", SizeBytes: 4096, SchemaVersion: 1, ContextCount: 2}, &statefile.CommitOutcomeUnknownError{Cause: errors.New("publication sync failed")}
					},
					recoverState: func(context.Context, string, string, bool) (sessionstate.StateRecoveryResult, error) {
						return sessionstate.StateRecoveryResult{Source: "/fixture/backup", Database: "/fixture/state/state.sqlite3", Previous: "/fixture/previous", PreviousValid: true}, sessionstate.ErrStateRecoveryPending
					},
				}
				args := []string{"state", "backup", "--output", "/fixture/backup"}
				if subcommand == "recover" {
					args = []string{"state", "recover", "--from", "/fixture/backup", "--yes"}
				}
				if structured {
					args = append([]string{"--json"}, args...)
				}
				var out, errout bytes.Buffer
				if code := runWith(args, strings.NewReader(""), &out, &errout, deps); code != exitOperation {
					t.Fatalf("code=%d: %s %s", code, &out, &errout)
				}
				if structured {
					var result commandResult
					if err := json.Unmarshal(out.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if subcommand == "recover" {
						if result.StateRecovery == nil || result.StateRecovery.Previous != "/fixture/previous" || result.StateRecovery.Applied {
							t.Fatalf("lost recovery evidence: %s", &out)
						}
						assertStateDiagnostic(t, errout.Bytes(), "state_recovery_pending")
					} else {
						if result.StateBackup == nil || result.StateBackup.Path != "/fixture/backup" {
							t.Fatalf("lost backup evidence: %s", &out)
						}
						assertStateDiagnostic(t, errout.Bytes(), "state_backup")
					}
				} else if subcommand == "recover" {
					if !strings.Contains(out.String(), `Rollback location: "/fixture/previous"`) || !strings.Contains(out.String(), "Recovery did not complete") || strings.Contains(out.String(), "Recovered") {
						t.Fatalf("misleading recovery failure: %s", &out)
					}
				} else if !strings.Contains(out.String(), "publication could not be confirmed") || strings.Contains(out.String(), "Backed up") {
					t.Fatalf("misleading backup failure: %s", &out)
				}
			})
		}
	}
}

func TestStateDefaultDependenciesBackupAndRecovery(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state-home"))
	t.Setenv("XDG_RUNTIME_DIR", "") // Backup and preview must not require runtime setup.
	deps := defaultDependencies(strings.NewReader(""))
	root, err := deps.stateRoot()
	if err != nil {
		t.Fatal(err)
	}
	deps.workingDir = func() (string, error) { return base, nil }
	storeRestoreTestContexts(t, deps, 1)
	backupDir := filepath.Join(base, "backups")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(backupDir, "metadata.sqlite3")
	var out, errout bytes.Buffer
	if code := runWith([]string{"state", "backup", "--output", output}, strings.NewReader(""), &out, &errout, deps); code != exitSuccess {
		t.Fatalf("backup code=%d: %s %s", code, &out, &errout)
	}
	if !strings.Contains(out.String(), "schema 1; 1 contexts") {
		t.Fatalf("missing backup summary: %s", &out)
	}
	if strings.Contains(out.String(), "Restore 1") || strings.Contains(out.String(), "restore-1") {
		t.Fatalf("private context in backup summary: %s", &out)
	}

	// The real preview operation may read only the portable input. Its target
	// can be absent, and it must not create a root, rollback file, or lock.
	target := filepath.Join(base, "recovered")
	deps.stateRoot = func() (string, error) { return target, nil }
	out.Reset()
	errout.Reset()
	if code := runWith([]string{"state", "recover", "--from", output}, strings.NewReader(""), &out, &errout, deps); code != exitSuccess {
		t.Fatalf("preview code=%d: %s %s", code, &out, &errout)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real preview created state root: %v", err)
	}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	out.Reset()
	errout.Reset()
	if code := runWith([]string{"--json", "state", "recover", "--from", output, "--yes"}, strings.NewReader(""), &out, &errout, deps); code != exitSuccess {
		t.Fatalf("apply code=%d: %s %s", code, &out, &errout)
	}
	var result commandResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.StateRecovery == nil || !result.StateRecovery.Applied || result.StateRecovery.Previous != "" {
		t.Fatalf("apply result: %s (%v)", &out, err)
	}
	var original sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(root).LoadInto(&original); err != nil {
		t.Fatal(err)
	}
	var recovered sessionstate.Registry
	if err := sessionstate.RegistryStoreFor(target).LoadInto(&recovered); err != nil {
		t.Fatal(err)
	}
	if len(recovered.Contexts) != 1 || recovered.Contexts[0].ID != original.Contexts[0].ID {
		t.Fatalf("fixture registration was not recovered: %+v", recovered)
	}
}

func TestStateRecoveryInstalledButCompletionUncertain(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			deps := dependencies{
				stateRoot: func() (string, error) { return "/fixture/state", nil },
				recoverState: func(context.Context, string, string, bool) (sessionstate.StateRecoveryResult, error) {
					return sessionstate.StateRecoveryResult{Source: "/fixture/backup", Database: "/fixture/state/state.sqlite3", Applied: true, Resumed: true}, &statefile.CommitOutcomeUnknownError{Cause: errors.New("completion sync failed")}
				},
			}
			args := []string{"state", "recover", "--from", "/fixture/backup", "--yes"}
			if structured {
				args = append([]string{"--json"}, args...)
			}
			var out, errout bytes.Buffer
			if code := runWith(args, strings.NewReader(""), &out, &errout, deps); code != exitOperation {
				t.Fatalf("code=%d: %s %s", code, &out, &errout)
			}
			if structured {
				var result commandResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.StateRecovery == nil || !result.StateRecovery.Applied || !result.StateRecovery.Resumed {
					t.Fatalf("lost installation evidence: %s (%v)", &out, err)
				}
				assertStateDiagnostic(t, errout.Bytes(), "state_recovery")
			}
			if !strings.Contains(out.String(), "replacement installed") || !strings.Contains(out.String(), "completion could not be confirmed") || strings.Contains(out.String(), "unchanged") {
				t.Fatalf("uncertain outcome misreported: %s", &out)
			}
		})
	}
}

func TestStateHelpHasNoEffects(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help", "state"}, {"state", "--help"}, {"state", "backup", "--help"}, {"state", "recover", "--help"}} {
		var out, errout bytes.Buffer
		if code := runWith(args, strings.NewReader(""), &out, &errout, dependencies{}); code != exitSuccess || errout.Len() != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, &errout)
		}
		if !strings.Contains(out.String(), "state") {
			t.Fatalf("missing state help: %s", &out)
		}
		if len(args) > 1 {
			for _, phrase := range []string{"backup --output PATH", "recover --from PATH", "--yes", "read-only preview", "0700", "0600", "older CLI and broker", "mixed-version", "resume"} {
				if !strings.Contains(out.String(), phrase) {
					t.Fatalf("missing %q: %s", phrase, &out)
				}
			}
		}
	}
}

func assertStateDiagnostic(t *testing.T, data []byte, code string) {
	t.Helper()
	var envelope struct {
		Diagnostics []diagnostic.Diagnostic `json:"diagnostics"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != code {
		t.Fatalf("want %s diagnostic, got %s (%v)", code, data, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("extra non-JSON output: %s", data)
	}
}
