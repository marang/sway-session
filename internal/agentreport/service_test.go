package agentreport

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

func TestRegistryServiceRoutesMultipleAgentKindsToFixedContextLauncher(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	contextID, err := sessionstate.ParseContextID("123e4567-e89b-12d3-a456-426614174000")
	if err != nil {
		t.Fatal(err)
	}
	registered := sessionstate.Context{
		ID: contextID, State: sessionstate.ContextActive,
		Launcher: sessionstate.Launcher{Kind: sessionstate.LauncherHerdr, Session: "lab-122", Cwd: t.TempDir(), Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty}},
	}
	if _, err := sessionstate.UpdateRegistry(stateRoot, func(registry *sessionstate.Registry) error {
		return sessionstate.AddContext(registry, registered)
	}); err != nil {
		t.Fatal(err)
	}
	paths := sessionstate.HerdrPaths{Root: "/fixed/herdr"}
	now := time.Unix(123, 0)
	var routed []string
	service := RegistryService{
		StateRoot: stateRoot, HerdrPaths: paths, Now: func() time.Time { return now },
		Report: func(_ context.Context, gotPaths sessionstate.HerdrPaths, launcher sessionstate.Launcher, paneID string, agent string, sessionID string, peerPID int, gotNow time.Time) error {
			if gotPaths != paths || !reflect.DeepEqual(launcher, registered.Launcher) || paneID != "work:p1" || peerPID != 4242 || gotNow != now {
				t.Fatalf("generic broker changed fixed routing data: paths=%+v launcher=%+v pane=%q pid=%d now=%v", gotPaths, launcher, paneID, peerPID, gotNow)
			}
			routed = append(routed, agent+":"+sessionID)
			return nil
		},
	}
	for _, report := range []Report{
		{Version: ProtocolVersion, ContextID: contextID, PaneID: "work:p1", Agent: "claude", AgentSessionID: "claude:thread-1", PeerPID: 4242},
		{Version: ProtocolVersion, ContextID: contextID, PaneID: "work:p1", Agent: "opencode", AgentSessionID: "opencode:thread-2", PeerPID: 4242},
	} {
		if err := service.Handle(context.Background(), report); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"claude:claude:thread-1", "opencode:opencode:thread-2"}; !reflect.DeepEqual(routed, want) {
		t.Fatalf("unexpected generic reports: got=%v want=%v", routed, want)
	}
}
