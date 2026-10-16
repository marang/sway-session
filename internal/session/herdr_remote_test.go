package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const remoteTestMachineID = "0123456789abcdef0123456789abcdef"
const remoteTestOtherMachineID = "1123456789abcdef0123456789abcdef"

func remoteMachineRow(id, label, target, sessionName string, enabled bool) string {
	value := "false"
	if enabled {
		value = "true"
	}
	return `{"id":"` + id + `","label":"` + label + `","target":"` + target + `","session":"` + sessionName + `","enabled":` + value + `,"selected":false}`
}

type remoteMachineRunner struct {
	output string
	err    error
	calls  int
	check  func(context.Context, string, []string)
}

func (r *remoteMachineRunner) CombinedOutput(ctx context.Context, exe string, args ...string) ([]byte, error) {
	r.calls++
	if r.check != nil {
		r.check(ctx, exe, args)
	}
	return []byte(r.output), r.err
}

func TestResolveRemoteMachinePinsIDTargetAndSession(t *testing.T) {
	row := remoteMachineRow(remoteTestMachineID, "Build", "host-a", "same-session", true)
	runner := &remoteMachineRunner{output: "[" + row + "]", check: func(ctx context.Context, exe string, args []string) {
		if exe != "/usr/bin/herdr" || !reflect.DeepEqual(args, []string{"machine", "list", "--json"}) {
			t.Fatalf("unexpected process %s %v", exe, args)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > herdrRemoteCatalogTimeout {
			t.Fatal("catalog has no bounded deadline")
		}
	}}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Runner: runner}
	want := TerminalRemoteBinding{MachineID: remoteTestMachineID, Target: "host-a", Session: "same-session"}
	for _, selector := range []string{"Build", remoteTestMachineID} {
		got, err := manager.ResolveRemoteMachine(t.Context(), selector)
		if err != nil || got != want {
			t.Fatalf("selector %s: %+v %v", selector, got, err)
		}
	}
	// Renaming and changes to global desired selection cannot retarget a pin.
	runner.output = "[" + remoteMachineRow(remoteTestMachineID, "Renamed", "host-a", "same-session", true) + "]"
	if err := manager.ValidateRemoteBinding(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{remoteMachineRow(remoteTestMachineID, "Renamed", "host-b", "same-session", true), remoteMachineRow(remoteTestMachineID, "Renamed", "host-a", "other-session", true), remoteMachineRow(remoteTestMachineID, "Renamed", "host-a", "same-session", false)} {
		runner.output = "[" + changed + "]"
		if err := manager.ValidateRemoteBinding(t.Context(), want); err == nil {
			t.Fatal("changed saved route accepted")
		}
	}
}

func TestResolveRemoteMachineRefusesAmbiguityAndMalformedCatalog(t *testing.T) {
	first := remoteMachineRow(remoteTestMachineID, "Build", "host-a", "work", true)
	second := remoteMachineRow(remoteTestOtherMachineID, "Build", "host-b", "work", true)
	for _, tc := range []struct{ name, output, selector string }{
		{"ambiguous", "[" + first + "," + second + "]", "Build"},
		{"duplicate_id", "[" + first + "," + first + "]", remoteTestMachineID},
		{"missing", "[]", remoteTestMachineID},
		{"disabled", "[" + remoteMachineRow(remoteTestMachineID, "Build", "host-a", "work", false) + "]", remoteTestMachineID},
		{"null", "null", remoteTestMachineID},
		{"trailing", "[" + first + "] {}", remoteTestMachineID},
		{"missing_fields", `[{"id":"` + remoteTestMachineID + `"}]`, remoteTestMachineID},
		{"invalid_id", "[" + remoteMachineRow("not-an-id", "Build", "host-a", "work", true) + "]", "Build"},
		{"overflow", strings.Repeat(" ", maxHerdrOutputSize+1), "Build"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := HerdrManager{Executable: "/usr/bin/herdr", Runner: &remoteMachineRunner{output: tc.output}}
			if _, err := manager.ResolveRemoteMachine(t.Context(), tc.selector); err == nil {
				t.Fatal("invalid catalog/selection accepted")
			}
		})
	}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Runner: &remoteMachineRunner{output: "[" + first + "," + remoteMachineRow(remoteTestOtherMachineID, remoteTestMachineID, "host-b", "work", true) + "]"}}
	got, err := manager.ResolveRemoteMachine(t.Context(), remoteTestMachineID)
	if err != nil || got.Target != "host-a" {
		t.Fatalf("ID lost precedence to label: %+v %v", got, err)
	}
}

func TestRemoteBindingDirectNeedsNoCatalogAndErrorsHideOutput(t *testing.T) {
	runner := &remoteMachineRunner{output: "private target", err: errors.New("private failure")}
	manager := HerdrManager{Executable: "/usr/bin/herdr", Runner: runner}
	if err := manager.ValidateRemoteBinding(t.Context(), TerminalRemoteBinding{Target: "host-a", Session: "work"}); err != nil || runner.calls != 0 {
		t.Fatalf("direct binding contacted catalog: %v, %d", err, runner.calls)
	}
	_, err := manager.ResolveRemoteMachine(t.Context(), remoteTestMachineID)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("diagnostic exposed process output: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner.err = nil
	runner.output = "[]"
	if _, err := manager.ResolveRemoteMachine(ctx, remoteTestMachineID); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
