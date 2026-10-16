package session

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/marang/sway-session/internal/swayipc"
)

func reachTestEnvironment(ids ...ContextID) WorkEnvironment {
	return WorkEnvironment{ID: EnvironmentID("00000000-0000-4000-8000-000000000375"), Name: "Development", Revision: 1, Members: ids, Primary: ids[0]}
}

func TestReachSharedExistingAppIsFocusedWithoutChangingIntent(t *testing.T) {
	registry := applicationInventoryTestRegistry("Shared browser")
	registry.Contexts[0].App.DesiredOpen = false
	before := registry.Contexts[0]
	presence := map[ContextID]ReachPresence{testContextID: {Known: true, ContainerID: 42}}
	for _, name := range []string{"Development", "Review"} {
		environment := reachTestEnvironment(testContextID)
		environment.Name = name
		preview, err := PlanWorkEnvironmentReach(t.Context(), environment, registry, presence, "")
		if err != nil || !preview.Ready || len(preview.Members) != 1 || preview.Members[0].Status != ReachWouldFocus {
			t.Fatalf("%s: %+v %v", name, preview, err)
		}
	}
	if !reflect.DeepEqual(before, registry.Contexts[0]) {
		t.Fatal("Reach changed shared application lifecycle/identity")
	}
	presence[testContextID] = ReachPresence{Known: true}
	preview, err := PlanWorkEnvironmentReach(t.Context(), reachTestEnvironment(testContextID), registry, presence, "")
	if err != nil || preview.Ready || preview.Members[0].Status != ReachSkipped {
		t.Fatalf("closed absent app was admitted: %+v %v", preview, err)
	}
}

func TestReachConservativeEvidenceAndExplicitPrimary(t *testing.T) {
	registry := validRegistry()
	environment := reachTestEnvironment(testContextID)
	for _, presence := range []ReachPresence{{}, {Known: true, Ambiguous: true}, {Known: true, Hidden: true, ContainerID: 42}, {Known: true, Restoring: true, ContainerID: 42}, {Known: true}} {
		preview, err := PlanWorkEnvironmentReach(t.Context(), environment, registry, map[ContextID]ReachPresence{testContextID: presence}, "")
		if err != nil || preview.Ready || preview.Members[0].Status == ReachWouldFocus || preview.Members[0].Status == ReachWouldOpen {
			t.Fatalf("unsafe primary admitted: %+v %v", preview, err)
		}
	}
	registry.Contexts[0].State = ContextArchived
	preview, err := PlanWorkEnvironmentReach(t.Context(), environment, registry, map[ContextID]ReachPresence{testContextID: {Known: true, ContainerID: 42}}, "")
	if err != nil || preview.Ready || preview.Members[0].Status != ReachSkipped {
		t.Fatalf("archived present primary admitted: %+v %v", preview, err)
	}
	registry.Contexts = []Context{}
	preview, err = PlanWorkEnvironmentReach(t.Context(), environment, registry, map[ContextID]ReachPresence{}, "")
	if err != nil || preview.Ready || preview.Members[0].Status != ReachUncertain {
		t.Fatalf("deleted reference guessed: %+v %v", preview, err)
	}
}

func TestReachOverrideDoesNotEditSavedPrimary(t *testing.T) {
	registry := validRegistry()
	second := registry.Contexts[0]
	second.ID = secondContextID
	second.Launcher.Session = "second"
	registry.Contexts = append(registry.Contexts, second)
	environment := reachTestEnvironment(testContextID, secondContextID)
	presence := map[ContextID]ReachPresence{testContextID: {Known: true}, secondContextID: {Known: true, ContainerID: 42}}
	preview, err := PlanWorkEnvironmentReach(t.Context(), environment, registry, presence, secondContextID)
	if err != nil || !preview.Ready || preview.Primary != secondContextID || environment.Primary != testContextID || preview.Members[0].Status != ReachBlocked || preview.Members[1].Status != ReachWouldFocus {
		t.Fatalf("override changed intent or missed selected member: %+v %v", preview, err)
	}
	if _, err := PlanWorkEnvironmentReach(t.Context(), reachTestEnvironment(testContextID), registry, presence, secondContextID); err == nil {
		t.Fatal("accepted nonmember primary")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PlanWorkEnvironmentReach(ctx, environment, registry, presence, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preview: %v", err)
	}
}

func TestReachCanonicalObservationNeverChoosesDuplicateTerminal(t *testing.T) {
	registry := validRegistry()
	appID, _ := testContextID.AppID()
	leaf := func(id int64) *swayipc.TreeNode { return &swayipc.TreeNode{ID: id, Type: "con", AppID: &appID} }
	workspace := &swayipc.TreeNode{ID: 98, Type: "workspace", Name: "98", Nodes: []*swayipc.TreeNode{leaf(41), leaf(42)}}
	root := &swayipc.TreeNode{ID: 1, Type: "root", Nodes: []*swayipc.TreeNode{workspace}}
	observed, err := ObserveReachPresence(root, registry)
	if err != nil || !observed[testContextID].Ambiguous || observed[testContextID].ContainerID != 0 {
		t.Fatalf("duplicate anchor guessed: %+v %v", observed, err)
	}
	workspace.Nodes = workspace.Nodes[:1]
	observed, err = ObserveReachPresence(root, registry)
	if err != nil || observed[testContextID].ContainerID != 41 || !observed[testContextID].Known {
		t.Fatalf("unique terminal lost: %+v %v", observed, err)
	}
	workspace.Name = RestoreStagingWorkspace
	observed, err = ObserveReachPresence(root, registry)
	if err != nil || !observed[testContextID].Restoring {
		t.Fatalf("restore staging treated as reachable work: %+v %v", observed, err)
	}
}
