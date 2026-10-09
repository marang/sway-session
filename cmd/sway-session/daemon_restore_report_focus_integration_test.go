package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marang/sway-session/internal/buildmetadata"
	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

// SWAY_SESSION_HEADLESS_INTEGRATION=1 go test ./cmd/sway-session -run '^TestDaemonRestoreReportInactiveWorkspaceFocusHeadless$' -count=1 -v
// SWAY_SESSION_RESTORE_REPORT_DAEMON optionally selects an already-built binary
// for historical comparisons; otherwise build the current source. Everything
// the executable sees belongs to the private compositor and disposable roots.
func TestDaemonRestoreReportInactiveWorkspaceFocusHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway/Alacritty integration")
	}
	for _, name := range []string{"go", "sway", "alacritty", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("headless integration requires %s: %v", name, err)
		}
	}
	executable := os.Getenv("SWAY_SESSION_RESTORE_REPORT_DAEMON")
	if executable == "" {
		executable = filepath.Join(t.TempDir(), "sway-session")
		// This synthetic stamp identifies a test build, not a historical release.
		buildLifecycleDaemon(t, executable, buildmetadata.Metadata{
			Version: "lab277-headless", Commit: strings.Repeat("a", 40), Modified: true,
		})
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("built daemon: %s; SHA256=%x", executable, sha256.Sum256(binary))

	h := newRestoreCleanupHeadless(t)
	// The executable must not observe the workstation's logind/system bus.
	h.env = append(h.env, "DBUS_SYSTEM_BUS_ADDRESS=unix:path="+filepath.Join(h.root, "no-system-bus"))
	ids := []sessionstate.ContextID{
		testManagedContextID,
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b811-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b812-9dad-11d1-80b4-00c04fd430c8",
		"6ba7b813-9dad-11d1-80b4-00c04fd430c8",
	}
	registry := sessionRegistryIDs(ids...)
	for index := range registry.Contexts {
		registry.Contexts[index].Launcher.Cwd = h.root
	}
	if err := sessionstate.RegistryStoreFor(h.state).Save(registry); err != nil {
		t.Fatal(err)
	}
	windows := make([]int64, len(ids))
	for index, id := range ids {
		if index%2 == 0 {
			h.command(fmt.Sprintf("workspace %d", 98+index/2))
		}
		windows[index] = h.terminal(id)
		mark, err := id.Mark()
		if err != nil {
			t.Fatal(err)
		}
		// Already-marked windows need no adoption, launch, or reconstruction.
		h.command(fmt.Sprintf(`[con_id=%d] mark --add "%s"`, windows[index], mark))
		if index%2 == 1 {
			h.command("layout tabbed")
		}
	}
	originalFocus := windows[len(windows)-1]
	tree := h.tree()
	if got := focusedContainerID(tree); got != originalFocus {
		t.Fatalf("fixture global focus = %d, want singleton %d on workspace 100", got, originalFocus)
	}
	saved, err := sessionstate.CaptureLayout(tree, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Workspaces) != 3 {
		t.Fatalf("fixture must contain two tab pairs and one singleton: %+v", saved)
	}
	focusedContexts := map[string]sessionstate.ContextID{"98": ids[1], "99": ids[3], "100": ids[4]}
	for _, workspace := range saved.Workspaces {
		focused, expected := focusedContexts[workspace.Name]
		if !expected || workspace.RestoreMode != sessionstate.WorkspaceRestoreLayout || workspace.FocusedContext == nil || *workspace.FocusedContext != focused {
			t.Fatalf("fixture must retain local focus on each private workspace: %+v", workspace)
		}
		if workspace.Name != "100" && (workspace.Tiling == nil || workspace.Tiling.Layout != sessionstate.LayoutTabbed || len(workspace.Tiling.Children) != 2) {
			t.Fatalf("fixture workspace %s must contain a tab pair: %+v", workspace.Name, workspace)
		}
	}
	if err := sessionstate.LayoutStoreFor(h.state).Save(saved); err != nil {
		t.Fatal(err)
	}
	savedJSON, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}

	events := restoreReportFocusEvents(t, h)
	process := startLifecycleDaemon(t, h, executable)
	awaitLifecycleDaemon(t, h, process, windows)
	store := sessionstate.RestoreReportStoreFor(h.state)
	var report sessionstate.RestoreReport
	h.until("all automatic restore report records", func() bool {
		var err error
		report, err = store.LoadContext(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(report.Outcomes) == len(ids)
	})
	runID := report.AutomaticRunID
	// Observe beyond every row's real deadline, without advancing an injected
	// clock or focusing another workspace to make the old planner return Done.
	deadline := time.Time{}
	for _, outcome := range report.Outcomes {
		deadline = maxRestoreReportFocusTime(deadline, outcome.StartedAt.Add(restoreReportTimeout+2*time.Second))
	}
	assertStable := func() {
		t.Helper()
		select {
		case <-process.done:
			t.Fatalf("private daemon exited during report observation: %v", process.err)
		default:
		}
		tree := h.tree()
		if got := focusedContainerID(tree); got != originalFocus {
			t.Fatalf("daemon changed global focus: got %d, want %d on workspace 100", got, originalFocus)
		}
		captured, err := sessionstate.CaptureLayout(tree, registry)
		if err != nil {
			t.Fatal(err)
		}
		capturedJSON, err := json.Marshal(captured)
		if err != nil || !bytes.Equal(capturedJSON, savedJSON) {
			t.Fatalf("daemon changed exact live layout/local focus: captured=%s saved=%s err=%v", capturedJSON, savedJSON, err)
		}
		var persisted sessionstate.LayoutSnapshot
		if err := sessionstate.LayoutStoreFor(h.state).LoadIntoContext(h.ctx, &persisted); err != nil {
			t.Fatal(err)
		}
		persistedJSON, err := json.Marshal(persisted)
		if err != nil || !bytes.Equal(persistedJSON, savedJSON) {
			t.Fatalf("daemon changed exact durable layout/local focus: persisted=%s saved=%s err=%v", persistedJSON, savedJSON, err)
		}
		assertRestoreFocusNoTemporaryState(t, tree)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	assertStable()
	for waiting := true; waiting; {
		select {
		case event := <-events:
			assertRestoreReportFocusEvent(t, event)
		case <-ticker.C:
			assertStable()
		case <-timer.C:
			waiting = false
		case <-h.ctx.Done():
			t.Fatalf("private fixture expired before restore-report deadline: %v", h.ctx.Err())
		}
	}
	// A tick barrier proves all preceding focus events were inspected, even if
	// a forbidden focus excursion happened between sampled GET_TREE responses.
	barrier := "lab277-report-focus-final"
	message, err := h.client.RequestContext(h.ctx, swayipc.SendTick, []byte(barrier))
	if err == nil {
		err = swayipc.CheckSendTickResponse(message)
	}
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			assertRestoreReportFocusEvent(t, event)
			if event.Type == swayipc.EventTick && event.Payload == barrier {
				goto observed
			}
		case <-h.ctx.Done():
			t.Fatal("private event subscription did not reach final tick barrier")
		}
	}

observed:
	assertStable()
	report, err = store.LoadContext(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.AutomaticRunID != runID || len(report.Outcomes) != len(ids) {
		t.Fatalf("automatic restore report changed run or lost records: %+v", report)
	}
	for _, outcome := range report.Outcomes {
		if outcome.Source != "automatic" || outcome.OwnerRunID != runID || outcome.Status != "completed" || outcome.Reason != "restore_complete" || !outcome.WindowMapped || !outcome.PlacementApplied || !outcome.LayoutApplied {
			t.Errorf("already-correct workspace did not complete after real deadline: context=%s workspace=%s status=%s reason=%s mapped=%t placement=%t layout=%t", outcome.ContextID, outcome.Requested.Workspace, outcome.Status, outcome.Reason, outcome.WindowMapped, outcome.PlacementApplied, outcome.LayoutApplied)
		}
	}
	t.Logf("observed all %d automatic records beyond %s with exact live/durable snapshots, no focus events, and global container %d on workspace 100", len(ids), restoreReportTimeout, originalFocus)
	process.stop(t)
}

func maxRestoreReportFocusTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func restoreReportFocusEvents(t *testing.T, h *restoreCleanupHeadless) <-chan swayipc.Event {
	t.Helper()
	events := make(chan swayipc.Event, 256)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		swayipc.StreamSessionEvents(h.socket, events, done)
	}()
	t.Cleanup(func() {
		close(done)
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("private report-focus event subscription did not stop")
		}
	})
	select {
	case event := <-events:
		if event.Type != swayipc.EventStream || event.Change != "ready" {
			t.Fatalf("unexpected initial private subscription event: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("private report-focus event subscription not ready")
	}
	return events
}

func assertRestoreReportFocusEvent(t *testing.T, event swayipc.Event) {
	t.Helper()
	if event.Type == swayipc.EventShutdown || event.Type == swayipc.EventStream {
		t.Fatalf("private report-focus event stream lost continuity: %+v", event)
	}
	if (event.Type == swayipc.EventWindow || event.Type == swayipc.EventWorkspace) && event.Change == "focus" {
		t.Fatalf("already-correct restore report caused a focus event: %+v", event)
	}
}
