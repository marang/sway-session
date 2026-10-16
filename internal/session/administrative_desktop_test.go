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
)

func administrativeStateFixture() Context {
	return Context{
		ID: testContextID, State: ContextActive,
		Launcher: Launcher{Kind: LauncherDesktop, DesktopID: "org.example.Admin.desktop", DesktopOrigin: DesktopEntrySystem,
			DesktopPath: "/usr/share/applications/org.example.Admin.desktop",
			Administrative: &AdministrativeDesktopApproval{
				DesktopEntrySHA256: strings.Repeat("a", 64), HelperPath: "/usr/bin/pkexec",
				HelperSHA256: strings.Repeat("b", 64), ExecutablePath: "/usr/bin/true", ExecutableSHA256: strings.Repeat("c", 64),
			}},
		App: &Application{Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "org.example.Admin"},
			DesiredOpen: true, RestorePolicy: ApplicationRestorePinned},
	}
}

func TestAdministrativeRestoreGuardSurvivesRestartWithoutAnotherPrompt(t *testing.T) {
	for _, policy := range []ApplicationRestorePolicy{ApplicationRestoreFollow, ApplicationRestorePinned} {
		t.Run(string(policy), func(t *testing.T) {
			item := administrativeStateFixture()
			item.App.RestorePolicy = policy
			registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{item}}
			root := filepath.Join(t.TempDir(), "state")
			if err := RegistryStoreFor(root).Save(registry); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			options := ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: time.Second, MaxConcurrent: 2}
			state := ApplicationSessionState{Version: ApplicationSessionSchemaVersion, CompositorID: strings.Repeat("a", 64), Attempts: []ApplicationLaunchAttempt{}}
			coordinator, err := NewApplicationRestoreCoordinator(state.CompositorID, state, now, options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := coordinator.Plan(registry, nil, now.Add(time.Second))
			if err != nil || len(plan.Launch) != 1 {
				t.Fatalf("opted-in desired work did not get its startup attempt: %+v, %v", plan, err)
			}
			state, err = coordinator.BeginAttempt(item.ID, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err := ApplicationSessionStoreFor(root).Save(state); err != nil {
				t.Fatal(err)
			}
			var saved ApplicationSessionState
			if err := ApplicationSessionStoreFor(root).LoadInto(&saved); err != nil {
				t.Fatal(err)
			}
			// No window after dismissal, denial or late startup establishes no
			// safe second dispatch. A restarted daemon must retain that fact.
			restarted, err := NewApplicationRestoreCoordinator(state.CompositorID, saved, now.Add(time.Minute), options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err = restarted.Plan(registry, nil, now.Add(time.Minute+time.Second))
			if err != nil || len(plan.Launch) != 0 {
				t.Fatalf("timeout/restart rearmed an uncertain administrative attempt: %+v, %v", plan, err)
			}
			nextLogin, err := NewApplicationRestoreCoordinator(strings.Repeat("b", 64), saved, now.Add(time.Hour), options)
			if err != nil {
				t.Fatal(err)
			}
			plan, err = nextLogin.Plan(registry, nil, now.Add(time.Hour+time.Second))
			if err != nil || len(plan.Launch) != 1 {
				t.Fatalf("durable opt-in did not retain next-login policy: %+v, %v", plan, err)
			}
		})
	}
}

func TestAdministrativeInventoryExposesPermissionWithoutInventingAuthEvidence(t *testing.T) {
	snapshot := ApplicationInventorySnapshot{Registry: Registry{Version: ContextsSchemaVersion, Contexts: []Context{administrativeStateFixture()}}}
	item := BuildApplicationInventory(snapshot)[0]
	if !item.Launcher.Administrative || item.Presence.State != "unknown" || item.LauncherValidity.State != ApplicationLauncherUnknown {
		t.Fatalf("permission was lost or authentication/presence inferred: %+v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil || strings.Contains(string(encoded), "helper_path") || strings.Contains(string(encoded), "helper_sha256") {
		t.Fatal("inventory leaked private approval identity")
	}
}

func TestAdministrativeDesktopRequiresExplicitBoundedShape(t *testing.T) {
	for _, exec := range []string{
		"/usr/bin/pkexec /usr/bin/true",
		"pkexec /usr/bin/true --literal %U",
		"'/usr/bin/pkexec' /usr/bin/true", "'sudo' /usr/bin/true",
		`/usr/bin/pkexec\s/usr/bin/true`, `/usr/bin/sudo\t/usr/bin/true`,
		`"/usr/bin/pkexec" "/usr/bin/true" "literal argument"`,
		"sudo /usr/bin/true", "/usr/bin/sudo -A /usr/bin/true",
		"/usr/bin/sudo --askpass /usr/bin/true",
	} {
		t.Run(exec, func(t *testing.T) {
			entry := DesktopEntry{ID: "org.example.Admin.desktop", Name: "Admin", Path: "/usr/share/applications/org.example.Admin.desktop", Exec: exec, Origin: DesktopEntrySystem}
			if err := ValidateDesktopEntryForApproval(entry); err == nil || !strings.Contains(err.Error(), "--allow-administrative") {
				t.Fatalf("ordinary approval accepted administrative starter: %v", err)
			}
			options := DesktopApprovalOptions{AllowAdministrative: true}
			if err := ValidateDesktopEntryForApprovalWithOptions(entry, options); err != nil {
				t.Fatalf("explicit supported starter refused: %v", err)
			}
			summary, err := DesktopApprovalSummaryWithOptions(entry, options)
			if err != nil || !strings.Contains(summary, "may request authentication again") || strings.Contains(summary, "--literal") {
				t.Fatalf("incorrect bounded trust preview: %q, %v", summary, err)
			}
		})
	}
	for _, exec := range []string{
		"pkexec", "pkexec true", "pkexec --user root /usr/bin/true",
		"pkexec -- /usr/bin/true", "pkexec /usr/bin/sh -c true",
		"pkexec /usr/bin/env true", "sudo -S /usr/bin/true",
		"sudo -u root /usr/bin/true", "sudo -A -A /usr/bin/true",
		"sudo -As /usr/bin/true", "sudo --askpass=yes /usr/bin/true",
		"sudo VAR=secret /usr/bin/true", "/tmp/pkexec /usr/bin/true",
		"pkexec /usr/bin/../bin/true", "pkexec /usr/bin/%f",
		`pkexec "/usr/bin/true`,
		"sh -c true", "doas /usr/bin/true", "run0 /usr/bin/true",
	} {
		t.Run("reject "+exec, func(t *testing.T) {
			entry := DesktopEntry{ID: "org.example.Admin.desktop", Name: "Admin", Path: "/usr/share/applications/org.example.Admin.desktop", Exec: exec, Origin: DesktopEntrySystem}
			if err := ValidateDesktopEntryForApprovalWithOptions(entry, DesktopApprovalOptions{AllowAdministrative: true}); err == nil {
				t.Fatal("opt-in expanded beyond the supported direct helper shape")
			}
		})
	}
	for _, entry := range []DesktopEntry{
		{Exec: "pkexec /usr/bin/true", DBusActivatable: true},
		{Exec: "sudo /usr/bin/true", Terminal: true},
	} {
		if _, _, err := administrativeDesktopEntry(entry); err == nil {
			t.Fatal("alternate activation route bypassed helper identity")
		}
	}
}

func TestAdministrativeApprovalDigestBindsEachTrustInput(t *testing.T) {
	approval := &AdministrativeDesktopApproval{
		DesktopEntrySHA256: strings.Repeat("a", 64), HelperPath: "/usr/bin/pkexec",
		HelperSHA256: strings.Repeat("b", 64), ExecutablePath: "/usr/bin/true", ExecutableSHA256: strings.Repeat("c", 64),
	}
	base, err := approval.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*AdministrativeDesktopApproval){
		func(a *AdministrativeDesktopApproval) { a.DesktopEntrySHA256 = strings.Repeat("d", 64) },
		func(a *AdministrativeDesktopApproval) { a.HelperPath = "/usr/bin/sudo" },
		func(a *AdministrativeDesktopApproval) { a.HelperSHA256 = strings.Repeat("d", 64) },
		func(a *AdministrativeDesktopApproval) { a.ExecutablePath = "/usr/bin/false" },
		func(a *AdministrativeDesktopApproval) { a.ExecutableSHA256 = strings.Repeat("d", 64) },
	} {
		copy := *approval
		change(&copy)
		got, err := copy.Digest()
		if err != nil || got == base {
			t.Fatalf("changed trust identity retained approval digest: %q, %v", got, err)
		}
	}
	var missing *AdministrativeDesktopApproval
	if _, err := missing.Digest(); err == nil {
		t.Fatal("missing approval acquired a fingerprint")
	}
}

func TestAdministrativeUserApprovalPinsTrustAndDoesNotLaunch(t *testing.T) {
	helper := ""
	for _, candidate := range []string{"/usr/bin/pkexec", "/usr/bin/sudo"} {
		if _, err := os.Stat(candidate); err == nil {
			helper = candidate
			break
		}
	}
	if helper == "" {
		t.Skip("optional administrative helpers are not installed; pure shape/opt-in/model tests remain active")
	}
	root := t.TempDir()
	path := filepath.Join(root, "org.example.Admin.desktop")
	data := []byte("[Desktop Entry]\nType=Application\nName=Admin\nExec=" + helper + " /usr/bin/true\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := parsedCatalogEntry(t, path, "org.example.Admin.desktop", DesktopEntryUser)
	options := DesktopApprovalOptions{AllowAdministrative: true}
	approval, err := PrepareDesktopApprovalContextWithOptions(context.Background(), root, testContextID, entry, options)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Launcher.Administrative == nil || approval.Launcher.ApprovedExecutablePath != "" {
		t.Fatal("administrative launch omitted root executable trust")
	}
	digest, err := AdministrativeDesktopApprovalDigest(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	preparedDigest, err := approval.Launcher.Administrative.Digest()
	if err != nil || digest != preparedDigest {
		t.Fatal("preview and prepared approval identities differ")
	}
	item := Context{ID: testContextID, State: ContextActive, Launcher: approval.Launcher,
		App: &Application{Identity: ApplicationIdentity{Protocol: WindowWayland, WaylandAppID: "org.example.Admin"}, DesiredOpen: true, RestorePolicy: ApplicationRestorePinned}}
	starter := &launcherRecordingStarter{}
	launcher := DesktopApplicationLauncher{GIO: "/usr/bin/gio", StateRoot: root, Starter: starter}
	spec, err := launcher.Spec(item)
	if err != nil {
		t.Fatal(err)
	}
	// pkexec requires a living parent through authentication. Starting GIO
	// here instead loses that parent before the authorization can complete.
	if spec.Name != helper || !reflect.DeepEqual(spec.Arguments, []string{"/usr/bin/true"}) {
		t.Fatalf("administrative dispatch must directly own the pinned helper: %+v", spec)
	}
	if starter.spec.Name != "" {
		t.Fatal("approval/observation started a process")
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Context
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Validate() != nil || decoded.Launcher.Administrative == nil {
		t.Fatal("administrative approval did not survive strict context validation")
	}
	item.Launcher.Administrative.HelperSHA256 = strings.Repeat("0", 64)
	observed := launcher.ObserveContext(context.Background(), item)
	if observed.State != ApplicationLauncherReapprovalRequired {
		t.Fatalf("changed helper evidence remained launchable: %+v", observed)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AdministrativeDesktopApprovalDigest(canceled, entry); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled trust inspection continued: %v", err)
	}
}

func TestAdministrativeDesktopFieldExpansionIsBoundedAndSinglePass(t *testing.T) {
	entry := DesktopEntry{Name: "Admin %U $(literal)", Icon: "an icon %c", Path: "/reviewed source.desktop"}
	for _, test := range []struct {
		exec string
		want []string
	}{
		{"pkexec /usr/bin/true %f %F %u %U", []string{"/usr/bin/true"}},
		{`sudo -A /usr/bin/true "two words" "" %%f %%%% %c %i %k`, []string{"-A", "/usr/bin/true", "two words", "", "%f", "%%", entry.Name, "--icon", entry.Icon, "/approved snapshot.desktop"}},
		{"pkexec /usr/bin/true --literal=%%c", []string{"/usr/bin/true", "--literal=%c"}},
	} {
		entry.Exec = test.exec
		shape, administrative, err := administrativeDesktopEntry(entry)
		if err != nil || !administrative {
			t.Fatalf("supported shape refused: %v", err)
		}
		got, err := administrativeDesktopArguments(shape, entry, "/approved snapshot.desktop")
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: got %q, %v; want %q", test.exec, got, err, test.want)
		}
	}
	entry.Exec, entry.Icon = "pkexec /usr/bin/true %i", ""
	shape, _, err := administrativeDesktopEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := administrativeDesktopArguments(shape, entry, entry.Path); err != nil || !reflect.DeepEqual(got, []string{"/usr/bin/true"}) {
		t.Fatalf("missing icon produced arguments: %q, %v", got, err)
	}
	for _, tail := range []string{"%", "%x", "%d", "--name=%c", "before%U", strings.Repeat("a ", 256), strings.Repeat("a", 64*1024)} {
		entry.Exec = "pkexec /usr/bin/true " + tail
		if _, _, err := administrativeDesktopEntry(entry); err == nil {
			t.Fatalf("unsupported/unbounded expansion was approved: %q", tail[:min(len(tail), 40)])
		}
	}
}

func TestAdministrativeDesktopRejectsWorkingDirectoryBeforeApproval(t *testing.T) {
	data := []byte("[Desktop Entry]\nType=Application\nName=Admin\nExec=/usr/bin/sudo /usr/bin/editor relative-file\nPath=/etc\n")
	entry, hidden, err := parseDesktopEntry(data)
	if err != nil || hidden || entry.WorkingDirectory != "/etc" {
		t.Fatalf("working-directory evidence lost: %+v, %v", entry, err)
	}
	entry.ID, entry.Origin, entry.Path = "org.example.Admin.desktop", DesktopEntryUser, "/user/applications/org.example.Admin.desktop"
	if err := ValidateDesktopEntryForApprovalWithOptions(entry, DesktopApprovalOptions{AllowAdministrative: true}); err == nil || !strings.Contains(err.Error(), "Path working directory") {
		t.Fatalf("direct administrative launch would silently change relative target: %v", err)
	}
	// Ordinary GIO entries retain working-directory semantics in their source.
	entry.Exec = "/usr/bin/editor relative-file"
	if err := ValidateDesktopEntryForApproval(entry); err != nil {
		t.Fatalf("ordinary GIO entry lost Path support: %v", err)
	}
}

func TestAdministrativeDesktopRequiresLiteralHashQuoting(t *testing.T) {
	entry := DesktopEntry{ID: "org.example.Admin.desktop", Name: "Admin", Path: "/usr/share/applications/org.example.Admin.desktop", Origin: DesktopEntrySystem}
	for _, tail := range []string{"#comment --flag", "--literal=#value", `""#comment`, `\t#comment`, `\\ #comment`} {
		entry.Exec = "/usr/bin/pkexec /usr/bin/true " + tail
		if err := ValidateDesktopEntryForApprovalWithOptions(entry, DesktopApprovalOptions{AllowAdministrative: true}); err == nil || !strings.Contains(err.Error(), "literal #") {
			t.Fatalf("ambiguous comment syntax was approved: %q, %v", tail, err)
		}
		entry.Exec = "/usr/bin/true " + tail
		if err := ValidateDesktopEntryForApproval(entry); err != nil {
			t.Fatalf("ordinary GIO entry lost existing parsing: %q, %v", tail, err)
		}
	}
	for _, tail := range []string{`"#literal --flag"`, `'--literal=#value'`, `\\#literal`} {
		entry.Exec = "/usr/bin/pkexec /usr/bin/true " + tail
		shape, _, err := administrativeDesktopEntry(entry)
		if err != nil {
			t.Fatalf("quoted/escaped literal hash refused: %q, %v", tail, err)
		}
		got, err := administrativeDesktopArguments(shape, entry, entry.Path)
		want := "#literal"
		if strings.HasPrefix(tail, `"`) {
			want = "#literal --flag"
		} else if strings.HasPrefix(tail, "'") {
			want = "--literal=#value"
		}
		if err != nil || !reflect.DeepEqual(got, []string{"/usr/bin/true", want}) {
			t.Fatalf("literal hash changed arguments: %q, %v", got, err)
		}
	}
}
