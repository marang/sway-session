package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUserDesktopApprovalCreatesProtectedSnapshotAndTypedGioLaunch(t *testing.T) {
	root := t.TempDir()
	applications := filepath.Join(root, "applications")
	if err := os.Mkdir(applications, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "approved-app")
	if err := os.WriteFile(executable, []byte("approved executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	desktopPath := filepath.Join(applications, "org.example.Local.desktop")
	desktopData := []byte("[Desktop Entry]\nType=Application\nName=Local\nExec=" + executable + " --literal='$(touch nope)' %U\nStartupWMClass=LocalApp\n")
	if err := os.WriteFile(desktopPath, desktopData, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := parsedCatalogEntry(t, desktopPath, "org.example.Local.desktop", DesktopEntryUser)
	stateRoot := filepath.Join(root, "state", "sway-session")

	approval, err := PrepareDesktopApproval(stateRoot, testContextID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Launcher.ApprovedDesktopPath == "" || approval.Launcher.ApprovedExecutablePath != executable {
		t.Fatalf("approval evidence is incomplete: %+v", approval)
	}
	info, err := os.Stat(approval.Launcher.ApprovedDesktopPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot is not owner-only: info=%v err=%v", info, err)
	}
	context := Context{
		ID: testContextID, State: ContextActive, Launcher: approval.Launcher,
		App: &Application{Identity: ApplicationIdentity{Protocol: WindowXWayland, X11Class: "LocalApp", X11Instance: "local", StartupWMClass: "LocalApp"}, RestorePolicy: ApplicationRestoreFollow},
	}
	starter := &launcherRecordingStarter{}
	launcher := DesktopApplicationLauncher{GIO: "/usr/bin/gio", Flatpak: "/usr/bin/flatpak", StateRoot: stateRoot, Starter: starter}
	if err := launcher.Launch(context); err != nil {
		t.Fatal(err)
	}
	if starter.spec.Name != "/usr/bin/gio" || !reflect.DeepEqual(starter.spec.Arguments, []string{"launch", approval.Launcher.ApprovedDesktopPath}) {
		t.Fatalf("unexpected GIO launch spec: %+v", starter.spec)
	}
	if !reflect.DeepEqual(starter.spec.Environment, []string{"PATH=/usr/local/bin:/usr/bin"}) {
		t.Fatalf("launch does not use the fixed trusted lookup path: %q", starter.spec.Environment)
	}

	if err := os.WriteFile(desktopPath, append(desktopData, []byte("Comment=updated\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := launcher.Spec(context); err == nil || !strings.Contains(err.Error(), "reapproval") {
		t.Fatalf("changed desktop entry was accepted: %v", err)
	}
	if err := os.WriteFile(desktopPath, desktopData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("changed executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := launcher.Spec(context); err == nil || !strings.Contains(err.Error(), "executable changed") {
		t.Fatalf("changed user executable was accepted: %v", err)
	}
}

func TestDesktopApplicationLauncherUsesOnlyTypedFlatpakArguments(t *testing.T) {
	context := flatpakApplicationContext("org.example.App", "org.example.App")
	context.ID = testContextID
	starter := &launcherRecordingStarter{}
	launcher := DesktopApplicationLauncher{GIO: "/usr/bin/gio", Flatpak: "/usr/bin/flatpak", Starter: starter, VerifyFlatpak: func(Launcher) error { return nil }}
	if err := launcher.Launch(context); err != nil {
		t.Fatal(err)
	}
	if starter.spec.Name != "/usr/bin/flatpak" || !reflect.DeepEqual(starter.spec.Arguments, []string{"run", "--user", "org.example.App"}) || !reflect.DeepEqual(starter.spec.Environment, []string{"PATH=/usr/local/bin:/usr/bin"}) {
		t.Fatalf("unexpected Flatpak launch spec: %+v", starter.spec)
	}
	launcher.Flatpak = filepath.Join(t.TempDir(), "flatpak")
	if err := launcher.Launch(context); err == nil {
		t.Fatal("non-system Flatpak executable was accepted")
	}
}

func TestVerifyFlatpakInstallationUsesExactTypedInfoArguments(t *testing.T) {
	runner := &desktopCommandRunner{}
	launcher := Launcher{Kind: LauncherFlatpak, FlatpakID: "org.example.App", FlatpakInstallation: FlatpakSystem}
	if err := VerifyFlatpakInstallation("/usr/bin/flatpak", launcher, runner); err != nil {
		t.Fatal(err)
	}
	if runner.name != "/usr/bin/flatpak" || !reflect.DeepEqual(runner.arguments, []string{"info", "--system", "org.example.App"}) {
		t.Fatalf("unexpected Flatpak verification: name=%q arguments=%q", runner.name, runner.arguments)
	}
	runner.err = errors.New("not installed")
	if err := VerifyFlatpakInstallation("/usr/bin/flatpak", launcher, runner); err == nil {
		t.Fatal("missing Flatpak installation was accepted")
	}
}

func TestDesktopEntryRejectsAdministrativeAndIndirectExecutables(t *testing.T) {
	for _, executable := range []string{"pkexec", "/usr/bin/sudo", "sh", "env", "python3", "node", "wine64"} {
		t.Run(executable, func(t *testing.T) {
			entry := DesktopEntry{Exec: executable + " harmless"}
			if err := rejectAdministrativeDesktopEntry(entry); err == nil {
				t.Fatalf("administrative executable %q was accepted", executable)
			}
		})
	}
	if err := rejectAdministrativeDesktopEntry(DesktopEntry{Exec: `"/opt/Example App/bin/app" --flag`}); err != nil {
		t.Fatalf("quoted direct executable was rejected: %v", err)
	}
}

func TestDesktopApprovalReuseIsNotDiscardedByFailedTransaction(t *testing.T) {
	root := t.TempDir()
	applications := filepath.Join(root, "applications")
	if err := os.Mkdir(applications, 0o700); err != nil {
		t.Fatal(err)
	}
	desktopPath := filepath.Join(applications, "org.example.Local.desktop")
	if err := os.WriteFile(desktopPath, []byte("[Desktop Entry]\nType=Application\nName=Local\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := parsedCatalogEntry(t, desktopPath, "org.example.Local.desktop", DesktopEntryUser)
	stateRoot := filepath.Join(root, "state")
	first, err := PrepareDesktopApproval(stateRoot, testContextID, entry)
	if err != nil || !first.SnapshotCreated {
		t.Fatalf("first approval did not create snapshot: approval=%+v err=%v", first, err)
	}
	second, err := PrepareDesktopApproval(stateRoot, testContextID, entry)
	if err != nil || second.SnapshotCreated {
		t.Fatalf("second approval did not reuse snapshot: approval=%+v err=%v", second, err)
	}
	if err := DiscardDesktopApproval(stateRoot, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.SnapshotPath); err != nil {
		t.Fatalf("discard of reused approval removed committed snapshot: %v", err)
	}
}

func TestUserDesktopApprovalRejectsSymlinkedTrustMaterial(t *testing.T) {
	root := t.TempDir()
	realDesktop := filepath.Join(root, "real.desktop")
	if err := os.WriteFile(realDesktop, []byte("[Desktop Entry]\nType=Application\nName=Local\nExec=/usr/bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedDesktop := filepath.Join(root, "linked.desktop")
	if err := os.Symlink(realDesktop, linkedDesktop); err != nil {
		t.Fatal(err)
	}
	entry := parsedCatalogEntry(t, realDesktop, "linked.desktop", DesktopEntryUser)
	entry.Path = linkedDesktop
	if _, err := PrepareDesktopApproval(filepath.Join(root, "state"), testContextID, entry); err == nil {
		t.Fatal("symlinked user desktop entry was accepted as stable trust material")
	}

	realExecutable := filepath.Join(root, "real-app")
	if err := os.WriteFile(realExecutable, []byte("app"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkedExecutable := filepath.Join(root, "linked-app")
	if err := os.Symlink(realExecutable, linkedExecutable); err != nil {
		t.Fatal(err)
	}
	desktopPath := filepath.Join(root, "executable.desktop")
	if err := os.WriteFile(desktopPath, []byte("[Desktop Entry]\nType=Application\nName=Local\nExec="+linkedExecutable+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry = parsedCatalogEntry(t, desktopPath, "executable.desktop", DesktopEntryUser)
	if _, err := PrepareDesktopApproval(filepath.Join(root, "state"), testContextID, entry); err == nil {
		t.Fatal("symlinked user executable was accepted as stable trust material")
	}
}

func TestRemoveDesktopApprovalSnapshotRejectsPathOutsideStateRoot(t *testing.T) {
	launcher := Launcher{ApprovedDesktopPath: filepath.Join(t.TempDir(), "foreign.desktop")}
	if err := RemoveDesktopApprovalSnapshot(filepath.Join(t.TempDir(), "state"), launcher); err == nil {
		t.Fatal("foreign approval snapshot path was accepted")
	}
}

func parsedCatalogEntry(t *testing.T, path string, id string, origin DesktopEntryOrigin) DesktopEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, hidden, err := parseDesktopEntry(data)
	if err != nil || hidden {
		t.Fatalf("parse fixture: hidden=%v err=%v", hidden, err)
	}
	entry.ID = id
	entry.Path = path
	entry.Origin = origin
	return entry
}

type desktopCommandRunner struct {
	name      string
	arguments []string
	err       error
}

func (runner *desktopCommandRunner) CombinedOutput(_ context.Context, name string, arguments ...string) ([]byte, error) {
	runner.name = name
	runner.arguments = append([]string(nil), arguments...)
	return nil, runner.err
}
