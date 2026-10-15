package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/statefile"
)

func TestApplicationLauncherObservationReusesUserApprovalValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, Context)
		want   ApplicationLauncherObservation
	}{
		{"valid", func(*testing.T, Context) {}, ApplicationLauncherObservation{ApplicationLauncherValid, "launcher_validated"}},
		{"missing source", func(t *testing.T, item Context) { removeObservationFixture(t, item.Launcher.DesktopPath) }, ApplicationLauncherObservation{ApplicationLauncherMissing, "desktop_entry_missing"}},
		{"changed source", func(t *testing.T, item Context) {
			writeObservationFixture(t, item.Launcher.DesktopPath, "changed source", 0o600)
		}, ApplicationLauncherObservation{ApplicationLauncherReapprovalRequired, "desktop_entry_changed"}},
		{"missing approval", func(t *testing.T, item Context) { removeObservationFixture(t, item.Launcher.ApprovedDesktopPath) }, ApplicationLauncherObservation{ApplicationLauncherReapprovalRequired, "approval_missing_or_changed"}},
		{"changed approval", func(t *testing.T, item Context) {
			writeObservationFixture(t, item.Launcher.ApprovedDesktopPath, "changed approval", 0o600)
		}, ApplicationLauncherObservation{ApplicationLauncherReapprovalRequired, "approval_missing_or_changed"}},
		{"missing executable", func(t *testing.T, item Context) { removeObservationFixture(t, item.Launcher.ApprovedExecutablePath) }, ApplicationLauncherObservation{ApplicationLauncherMissing, "approved_executable_missing"}},
		{"changed executable", func(t *testing.T, item Context) {
			writeObservationFixture(t, item.Launcher.ApprovedExecutablePath, "changed executable", 0o700)
		}, ApplicationLauncherObservation{ApplicationLauncherReapprovalRequired, "approved_executable_changed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, item := applicationLauncherObservationFixture(t)
			test.change(t, item)
			before := readObservationFixtureFiles(t, launcher.StateRoot)
			got := launcher.ObserveContext(context.Background(), item)
			if got != test.want {
				t.Fatalf("observation=%+v, want %+v", got, test.want)
			}
			if after := readObservationFixtureFiles(t, launcher.StateRoot); !reflect.DeepEqual(after, before) {
				t.Fatalf("observation changed approval files: before=%v after=%v", before, after)
			}
			encoded, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encoded), launcher.StateRoot) || strings.Contains(string(encoded), "private-argument") || strings.Contains(string(encoded), "PATH=") {
				t.Fatalf("observation leaked private specification: %s, %v", encoded, err)
			}
		})
	}
}

func applicationLauncherObservationFixture(t *testing.T) (DesktopApplicationLauncher, Context) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "app")
	writeObservationFixture(t, executable, "approved executable", 0o700)
	desktopPath := filepath.Join(root, "app.desktop")
	writeObservationFixture(t, desktopPath, "[Desktop Entry]\nType=Application\nName=Private\nExec="+executable+" --private-argument=secret\n", 0o600)
	stateRoot := filepath.Join(root, "state")
	approval, err := PrepareDesktopApproval(stateRoot, testContextID, parsedCatalogEntry(t, desktopPath, "org.example.Private.desktop", DesktopEntryUser))
	if err != nil {
		t.Fatal(err)
	}
	item := flatpakApplicationContext("org.example.Private", "org.example.Private")
	item.ID = testContextID
	item.Launcher = approval.Launcher
	item.App.Identity.SandboxAppID = ""
	return DesktopApplicationLauncher{
		GIO: systemGIOExecutable, StateRoot: stateRoot,
		Starter:                  observationForbiddenStarter{t},
		VerifyFlatpak:            func(Launcher) error { t.Fatal("legacy launch verifier called"); return nil },
		ObservationResolveSystem: observationTrustedResolver,
	}, item
}

func writeObservationFixture(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func removeObservationFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func readObservationFixtureFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[path] = string(data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

type observationForbiddenStarter struct{ t *testing.T }

func (starter observationForbiddenStarter) Start(ProcessSpec) error {
	starter.t.Fatal("observation attempted to start application")
	return nil
}

func observationTrustedResolver(name string) (string, error) {
	return filepath.Join("/usr/bin", name), nil
}

func TestApplicationLauncherObservationClassifiesMissingSystemDesktop(t *testing.T) {
	id, err := NewContextID()
	if err != nil {
		t.Fatal(err)
	}
	item := flatpakApplicationContext("org.example.Private", "org.example.Private")
	item.ID = testContextID
	item.App.Identity.SandboxAppID = ""
	// No runner-specific parent directory may reject trust before ENOENT is
	// observed. The UUID path at the filesystem root does not create a file.
	item.Launcher = Launcher{Kind: LauncherDesktop, DesktopOrigin: DesktopEntrySystem, DesktopID: "org.example.Missing.desktop", DesktopPath: filepath.Join("/", "sway-session-missing-"+string(id)+".desktop")}
	launcher := DesktopApplicationLauncher{GIO: systemGIOExecutable, ObservationResolveSystem: observationTrustedResolver}
	if got := launcher.ObserveContext(context.Background(), item); got != (ApplicationLauncherObservation{ApplicationLauncherMissing, "desktop_entry_missing"}) {
		t.Fatalf("observation=%+v", got)
	}
	// An untrusted ancestor must still fail closed, even if its child is absent.
	parent := filepath.Join(t.TempDir(), "untrusted")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	item.Launcher.DesktopPath = filepath.Join(parent, "missing.desktop")
	if got := launcher.ObserveContext(context.Background(), item); got != (ApplicationLauncherObservation{ApplicationLauncherInvalid, "desktop_entry_untrusted"}) {
		t.Fatalf("untrusted ancestor observation=%+v", got)
	}
}

func TestApplicationLauncherObservationUsesTypedFlatpakEvidence(t *testing.T) {
	for _, installation := range []FlatpakInstallation{FlatpakUser, FlatpakSystem} {
		for _, test := range []struct {
			name    string
			infoErr error
			list    string
			listErr error
			want    ApplicationLauncherObservation
			calls   int
		}{
			{"installed", nil, "", nil, ApplicationLauncherObservation{ApplicationLauncherValid, "flatpak_installed"}, 1},
			{"removed", errors.New("exit status 1 private-path"), "org.example.Other\n", nil, ApplicationLauncherObservation{ApplicationLauncherMissing, "flatpak_not_installed"}, 2},
			{"empty installation", errors.New("exit status 1"), "", nil, ApplicationLauncherObservation{ApplicationLauncherMissing, "flatpak_not_installed"}, 2},
			{"info failure but listed", errors.New("exit status 1"), "org.example.App\n", nil, ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}, 2},
			{"ambiguous failure", errors.New("not installed secret"), "", errors.New("exit status 1"), ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}, 2},
			{"permission", os.ErrPermission, "", nil, ApplicationLauncherObservation{ApplicationLauncherUnknown, "permission_denied"}, 1},
			{"command absent", os.ErrNotExist, "", nil, ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}, 1},
			{"malformed listing", errors.New("exit status 1"), "Permission denied /private/path\n", nil, ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}, 2},
			{"truncated listing", errors.New("exit status 1"), strings.Repeat("x", maxHerdrOutputSize), nil, ApplicationLauncherObservation{ApplicationLauncherUnknown, "flatpak_probe_unavailable"}, 2},
		} {
			t.Run(string(installation)+"/"+test.name, func(t *testing.T) {
				item := flatpakApplicationContext("org.example.App", "org.example.App")
				item.ID = testContextID
				item.Launcher.FlatpakInstallation = installation
				runner := &applicationLauncherObservationRunner{infoErr: test.infoErr, list: []byte(test.list), listErr: test.listErr}
				launcher := DesktopApplicationLauncher{Flatpak: systemFlatpakExecutable, ObservationRunner: runner, ObservationResolveSystem: observationTrustedResolver, Starter: observationForbiddenStarter{t}, VerifyFlatpak: func(Launcher) error { t.Fatal("legacy verifier called"); return nil }}
				if got := launcher.ObserveContext(context.Background(), item); got != test.want {
					t.Fatalf("observation=%+v, want %+v", got, test.want)
				}
				flag := "--" + string(installation)
				wantCalls := [][]string{{systemFlatpakExecutable, "info", flag, item.Launcher.FlatpakID}}
				if test.calls == 2 {
					wantCalls = append(wantCalls, []string{systemFlatpakExecutable, "list", "--app", flag, "--columns=application"})
				}
				if !reflect.DeepEqual(runner.calls, wantCalls) {
					t.Fatalf("probe calls=%v, want %v", runner.calls, wantCalls)
				}
			})
		}
	}
}

type applicationLauncherObservationRunner struct {
	infoErr error
	list    []byte
	listErr error
	calls   [][]string
}

func (runner *applicationLauncherObservationRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("probe deadline missing")
	}
	runner.calls = append(runner.calls, append([]string{name}, args...))
	if args[0] == "info" {
		return []byte("private diagnostic"), runner.infoErr
	}
	return runner.list, runner.listErr
}

func TestApplicationLauncherObservationCancellationAndCanonicalCommand(t *testing.T) {
	item := flatpakApplicationContext("org.example.App", "org.example.App")
	item.ID = testContextID
	launcher := DesktopApplicationLauncher{Flatpak: systemFlatpakExecutable, ObservationRunner: cancelAwareDesktopCommandRunner{}, ObservationResolveSystem: observationTrustedResolver}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := launcher.ObserveContext(ctx, item); got != (ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_timeout"}) {
		t.Fatalf("timed out observation=%+v", got)
	}
	ctx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got := launcher.ObserveContext(ctx, item); got != (ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_canceled"}) {
		t.Fatalf("canceled observation=%+v", got)
	}
	launcher.Flatpak = "/private/bin/flatpak"
	if got := launcher.ObserveContext(context.Background(), item); got != (ApplicationLauncherObservation{ApplicationLauncherInvalid, "launcher_command_invalid"}) {
		t.Fatalf("noncanonical observation=%+v", got)
	}
	launcher.Flatpak = systemFlatpakExecutable
	launcher.ObservationResolveSystem = func(string) (string, error) { return "", errors.New("private path and command unavailable") }
	if got := launcher.ObserveContext(context.Background(), item); got != (ApplicationLauncherObservation{ApplicationLauncherUnknown, "launcher_command_unavailable"}) {
		t.Fatalf("unavailable command observation=%+v", got)
	}
	launcher.ObservationResolveSystem = func(string) (string, error) { return "/usr/local/bin/flatpak", nil }
	if got := launcher.ObserveContext(context.Background(), item); got != (ApplicationLauncherObservation{ApplicationLauncherInvalid, "launcher_command_invalid"}) {
		t.Fatalf("unexpected resolved command observation=%+v", got)
	}
}

func TestApplicationLauncherObservationApprovalLockDeadline(t *testing.T) {
	launcher, item := applicationLauncherObservationFixture(t)
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithPrivateDirectoryLockContext(context.Background(), filepath.Dir(item.Launcher.ApprovedDesktopPath), func(*statefile.LockedPrivateDirectory) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := launcher.ObserveContext(ctx, item); got != (ApplicationLauncherObservation{ApplicationLauncherUnknown, "observation_timeout"}) {
		t.Fatalf("locked approval observation=%+v", got)
	}
}
