package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/marang/sway-title-animator/internal/swayipc"
)

func TestFindPendingProcessLaunchesUsesTheExactTypedProcessSpec(t *testing.T) {
	procRoot := t.TempDir()
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
	spec := ProcessSpec{Name: "/usr/bin/foot", Arguments: []string{"--app-id=sway-session.example", "--", "/usr/bin/herdr", "--session", "example"}}
	writeCmdline("5252", append([]string{spec.Name}, spec.Arguments...))
	writeCmdline("5253", []string{spec.Name, "--app-id=sway-session.other", "--", "/usr/bin/herdr", "--session", "example"})

	pids, err := FindPendingProcessLaunches(procRoot, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pids, []int{5252}) {
		t.Fatalf("unexpected matching PIDs: %v", pids)
	}
	if _, err := FindPendingProcessLaunches(procRoot, ProcessSpec{Name: "foot"}); err == nil {
		t.Fatal("relative executable was accepted")
	}
}

func TestDetachedProcessCommandConfiguresANewSession(t *testing.T) {
	command, err := detachedProcessCommand(ProcessSpec{Name: "/usr/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid {
		t.Fatalf("launched application remains in the caller session: %+v", command.SysProcAttr)
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

func TestMergeEnvironmentOverridesExistingKeyWithoutDuplicates(t *testing.T) {
	merged, err := mergeEnvironment(
		[]string{"PATH=/untrusted", "LANG=C", "HERDR_ENV=1", "HERDR_SOCKET_PATH=/old", "CODEX_THREAD_ID=old"},
		[]string{"PATH=/usr/local/bin:/usr/bin", "TOKEN=value", "HERDR_CONFIG_PATH=/trusted/config.toml"},
		[]string{"CODEX_THREAD_ID"},
		[]string{"HERDR_"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C", "TOKEN=value", "HERDR_CONFIG_PATH=/trusted/config.toml"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("environment override was ambiguous: got=%q want=%q", merged, want)
	}
	if _, err := mergeEnvironment(nil, []string{"BROKEN"}, nil, nil); err == nil {
		t.Fatal("malformed environment entry was accepted")
	}
	if _, err := mergeEnvironment(nil, nil, nil, []string{""}); err == nil {
		t.Fatal("empty environment prefix was accepted")
	}
}
