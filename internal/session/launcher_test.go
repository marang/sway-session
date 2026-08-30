package session

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestAlacrittyHerdrLauncherPassesMetadataWithoutShellEvaluation(t *testing.T) {
	context := testValidContext(testContextID)
	context.Label = "--help; $(touch nope)"
	context.Launcher.Cwd = t.TempDir()
	starter := &launcherRecordingStarter{}
	launcher := AlacrittyHerdrLauncher{Alacritty: "/usr/bin/alacritty", Herdr: "/usr/bin/herdr", Starter: starter}

	if err := launcher.Launch(context); err != nil {
		t.Fatal(err)
	}
	wantArguments, err := AlacrittyHerdrArguments(context, "/usr/bin/herdr")
	if err != nil {
		t.Fatal(err)
	}
	if starter.spec.Name != "/usr/bin/alacritty" || !reflect.DeepEqual(starter.spec.Arguments, wantArguments) {
		t.Fatalf("unexpected typed launch name=%q arguments=%q", starter.spec.Name, starter.spec.Arguments)
	}
	if !slices.Contains(starter.spec.Arguments, "--title=--help; $(touch nope)") {
		t.Fatalf("leading-hyphen title was not encoded as data: %q", starter.spec.Arguments)
	}
	if !reflect.DeepEqual(starter.spec.Environment, []string{"SWAY_SESSION_CONTEXT_ID=" + string(context.ID)}) {
		t.Fatalf("context identity was not injected as typed environment: %q", starter.spec.Environment)
	}
}

func TestFindPendingAlacrittyLaunchesMatchesOnlyCompleteTypedArgv(t *testing.T) {
	procRoot := t.TempDir()
	context := testValidContext(testContextID)
	arguments, err := AlacrittyHerdrArguments(context, "/usr/bin/herdr")
	if err != nil {
		t.Fatal(err)
	}
	writeCmdline := func(pid string, values []string) {
		t.Helper()
		directory := filepath.Join(procRoot, pid)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte{}
		for _, value := range values {
			data = append(data, value...)
			data = append(data, 0)
		}
		if err := os.WriteFile(filepath.Join(directory, "cmdline"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCmdline("4242", append([]string{"/usr/bin/alacritty"}, arguments...))
	writeCmdline("4243", []string{"/usr/bin/alacritty", "--class", "other"})

	pids, err := FindPendingAlacrittyLaunches(procRoot, context, "/usr/bin/alacritty", "/usr/bin/herdr")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pids, []int{4242}) {
		t.Fatalf("unexpected matching PIDs: %v", pids)
	}
}

func TestObserveManagedWindowsIncludesRestoreStagingAndRejectsDuplicates(t *testing.T) {
	context := testValidContext(testContextID)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{context}}
	appID, _ := context.ID.AppID()
	staging := &swayipc.TreeNode{ID: 2, Type: "workspace", Name: RestoreStagingWorkspace, Nodes: []*swayipc.TreeNode{{ID: 3, Type: "con", AppID: &appID}}}
	root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{staging}}

	windows, err := ObserveManagedWindows(root, registry)
	if err != nil || windows[context.ID].Workspace != RestoreStagingWorkspace {
		t.Fatalf("staging observation failed: windows=%v err=%v", windows, err)
	}
	staging.Nodes = append(staging.Nodes, &swayipc.TreeNode{ID: 4, Type: "con", AppID: &appID})
	if _, err := ObserveManagedWindows(root, registry); err == nil {
		t.Fatal("expected duplicate managed identity rejection")
	}
}

func TestObserveManagedWindowsIsolatedKeepsIndependentContexts(t *testing.T) {
	first := testValidContext(testContextID)
	second := testValidContext(ContextID("6ba7b810-9dad-41d1-80b4-00c04fd430c8"))
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{first, second}}
	firstAppID, _ := first.ID.AppID()
	secondAppID, _ := second.ID.AppID()
	workspace := &swayipc.TreeNode{ID: 2, Type: "workspace", Name: "1", Nodes: []*swayipc.TreeNode{
		{ID: 3, Type: "con", AppID: &firstAppID},
		{ID: 4, Type: "con", AppID: &firstAppID},
		{ID: 5, Type: "con", AppID: &secondAppID},
	}}
	root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{workspace}}

	windows, issues, err := ObserveManagedWindowsIsolated(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ContextID != first.ID {
		t.Fatalf("unexpected isolated issues: %+v", issues)
	}
	if len(windows) != 1 || windows[second.ID].ContainerID != 5 {
		t.Fatalf("independent context was not retained: %+v", windows)
	}
}

type launcherRecordingStarter struct {
	spec ProcessSpec
}

func (starter *launcherRecordingStarter) Start(spec ProcessSpec) error {
	starter.spec = spec
	starter.spec.Arguments = append([]string(nil), spec.Arguments...)
	starter.spec.Environment = append([]string(nil), spec.Environment...)
	return nil
}

func TestResolveRootOwnedSystemExecutableIgnoresCallerPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resolved, err := ResolveRootOwnedSystemExecutable("sh")
	if err != nil {
		t.Fatalf("resolve root-owned system executable: %v", err)
	}
	if resolved == "" || resolved[0] != '/' {
		t.Fatalf("unexpected resolved path %q", resolved)
	}
	if _, err := ResolveRootOwnedSystemExecutable("../sh"); err == nil {
		t.Fatal("unsafe executable name was accepted")
	}
}
