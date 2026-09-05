package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
)

type cancellingDoctorOperations struct {
	fakeDoctorOperations
	started, release chan struct{}
}

func (operations *cancellingDoctorOperations) Apply(ctx context.Context, _ doctor.Plan) (doctor.FixResult, error) {
	close(operations.started)
	<-ctx.Done()
	<-operations.release // Model the repair engine finishing its rollback.
	return doctor.FixResult{}, ctx.Err()
}

func TestDoctorUIShutdownWaitsForRepairAndRejectsQueuedApply(t *testing.T) {
	operations := &cancellingDoctorOperations{started: make(chan struct{}), release: make(chan struct{})}
	guarded := &doctorUIOperations{doctorOperations: operations}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = guarded.Apply(ctx, doctor.Plan{}) }()
	<-operations.started
	cancel()
	stopped := make(chan struct{})
	go func() { guarded.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("UI stopped before rollback finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(operations.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("UI did not finish after rollback")
	}
	<-done
	if _, err := guarded.Apply(context.Background(), doctor.Plan{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late Apply: %v", err)
	}
}

func doctorUpdate(t *testing.T, model doctorModel, msg tea.Msg) (doctorModel, tea.Cmd) {
	t.Helper()
	updated, command := model.Update(msg)
	return updated.(doctorModel), command
}

func TestDoctorNoColorFilterKeepsLongInputCursorVisible(t *testing.T) {
	model := newDoctorModel(context.Background(), &fakeDoctorOperations{})
	model.width, model.height, model.noColor, model.filtering = 48, 16, true, true
	model.filter.SetValue(strings.Repeat("x", 90) + "界終")
	model.filter.CursorEnd()
	view := model.filterView()
	if !strings.Contains(view, "界終│") || ansi.StringWidth(view) > 44 || strings.Contains(view, "\x1b") {
		t.Fatalf("long filter lost visible cursor: %q", view)
	}
	model.filter.SetCursor(5)
	view = model.filterView()
	if !strings.Contains(view, "xxxxx│x") || ansi.StringWidth(view) > 44 {
		t.Fatalf("mid-input cursor invisible: %q", view)
	}
}

func TestDoctorTUIFilterRefreshSelectionAndNoColor(t *testing.T) {
	ops := &fakeDoctorOperations{report: doctor.Report{Checks: []doctor.Check{
		{ID: "first", Title: "First check", Status: doctor.OK},
		{ID: "second", Title: "Second check", Status: doctor.Warning, Detail: "Missing config", FixID: "sway.integration"},
	}}}
	model := newDoctorModel(context.Background(), ops)
	model.noColor = true
	model, _ = doctorUpdate(t, model, model.Init()())
	model, _ = doctorUpdate(t, model, terminalManageKey("j"))
	ops.report.Checks = []doctor.Check{ops.report.Checks[1], ops.report.Checks[0]}
	var command tea.Cmd
	model, command = doctorUpdate(t, model, terminalManageKey("r"))
	model, _ = doctorUpdate(t, model, command())
	if selected, _ := model.selected(); selected.ID != "second" {
		t.Fatalf("lost selected check: %+v", selected)
	}
	for _, size := range [][2]int{{80, 24}, {48, 16}, {120, 30}} {
		model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := model.View().Content
		if strings.Contains(view, "\x1b") || !strings.Contains(view, "[q] Quit") || !strings.Contains(view, "[warning]") {
			t.Fatalf("bad view %v:\n%s", size, view)
		}
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Fatalf("height=%d > %d", len(lines), size[1])
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("line exceeds width %d: %q", size[0], line)
			}
		}
	}
	model, _ = doctorUpdate(t, model, terminalManageKey("j"))
	model, _ = doctorUpdate(t, model, terminalManageKey("/"))
	if view := model.View().Content; strings.Contains(view, "\x1b[") {
		t.Fatalf("NO_COLOR filter contains ANSI styling: %q", view)
	}
	tiny, _ := doctorUpdate(t, model, tea.WindowSizeMsg{Width: 20, Height: 8})
	if _, command := doctorUpdate(t, tiny, terminalManageKey("q")); command == nil {
		t.Fatal("tiny fallback advertised q but filter captured it")
	}
	model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: 120, Height: 30})
	model, _ = doctorUpdate(t, model, terminalManageKey("check"))
	if selected, _ := model.selected(); selected.ID != "first" || len(model.visible) != 2 {
		t.Fatalf("filter lost matching selection: selected=%+v visible=%v", selected, model.visible)
	}
	model, _ = doctorUpdate(t, model, terminalManageKey("enter"))
	model, _ = doctorUpdate(t, model, terminalManageKey("esc"))
	model, _ = doctorUpdate(t, model, terminalManageKey("/"))
	model, _ = doctorUpdate(t, model, terminalManageKey("first"))
	model, _ = doctorUpdate(t, model, terminalManageKey("enter"))
	if selected, _ := model.selected(); selected.ID != "first" || len(model.visible) != 1 {
		t.Fatalf("filter failed: %+v", model.visible)
	}
	model, _ = doctorUpdate(t, model, terminalManageKey("esc"))
	if len(model.visible) != 2 {
		t.Fatal("filter did not clear")
	}
}

func TestDoctorTUIHelpAtMinimumSizeHasAccurateExitKeys(t *testing.T) {
	model := newDoctorModel(context.Background(), &fakeDoctorOperations{})
	model.busy = ""
	model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: 48, Height: 16})
	model, _ = doctorUpdate(t, model, terminalManageKey("?"))
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "[Esc/?] Close help") || !strings.Contains(view, "[q] Quit") {
		t.Fatalf("minimum-size help lost accurate recovery controls:\n%s", view)
	}
	updated, command := doctorUpdate(t, model, terminalManageKey("q"))
	if command == nil || !updated.help {
		t.Fatalf("q did not quit directly from help: help=%v command=%v", updated.help, command)
	}
}

func TestDoctorTUIRepairRequiresPreviewAndConfirmation(t *testing.T) {
	ops := &fakeDoctorOperations{
		report: doctor.Report{Checks: []doctor.Check{{ID: "sway.startup", Title: "Startup", Status: doctor.Warning, FixID: "sway.integration"}}},
		plan:   doctor.Plan{ID: "sway.integration", Summary: "Add missing integration", Changes: []doctor.FileChange{{Path: "/test/config", Preview: "+ include managed.conf"}}},
		applyResult: doctor.FixResult{
			Message: "Applied the managed Sway integration files. Reload Sway when convenient.",
			Backups: []string{"/home/test/.config/sway/config.backup", "/home/test/.config/sway/50-sway-session-doctor.conf.backup"},
		},
	}
	model := newDoctorModel(context.Background(), ops)
	model, _ = doctorUpdate(t, model, model.Init()())
	model, command := doctorUpdate(t, model, terminalManageKey("f"))
	if command == nil || ops.applyCalls != 0 {
		t.Fatal("fix did not prepare preview")
	}
	model, _ = doctorUpdate(t, model, command())
	if model.plan == nil || !strings.Contains(ansi.Strip(model.View().Content), "include managed.conf") {
		t.Fatal("missing preview")
	}
	model, _ = doctorUpdate(t, model, terminalManageKey("n"))
	if model.plan != nil || ops.applyCalls != 0 {
		t.Fatal("cancel mutated")
	}
	model, command = doctorUpdate(t, model, terminalManageKey("f"))
	model, _ = doctorUpdate(t, model, command())
	model, command = doctorUpdate(t, model, terminalManageKey("y"))
	if command == nil || ops.applyCalls != 0 {
		t.Fatal("Apply ran inside Update")
	}
	if _, quit := doctorUpdate(t, model, terminalManageKey("q")); quit != nil {
		t.Fatal("quit abandoned in-flight repair")
	}
	model, recheck := doctorUpdate(t, model, command())
	if ops.applyCalls != 1 || recheck == nil {
		t.Fatal("confirmed repair did not recheck")
	}
	model, _ = doctorUpdate(t, model, recheck())
	if model.busy != "" || model.plan != nil || len(model.feedback) == 0 {
		t.Fatal("repair did not converge to report")
	}
	view := ansi.Strip(model.View().Content)
	for _, backup := range ops.applyResult.Backups {
		if !strings.Contains(view, backup) {
			t.Fatalf("repair result hid backup %q:\n%s", backup, view)
		}
	}
	model, _ = doctorUpdate(t, model, terminalManageKey("esc"))
	if len(model.feedback) != 0 {
		t.Fatal("repair feedback did not close explicitly")
	}
}

func TestDoctorTUIPreviewScrollAndFailure(t *testing.T) {
	var preview strings.Builder
	for i := range 90 {
		fmt.Fprintf(&preview, "+ managed line %d\n", i)
	}
	recoveryPath := "/home/test/.config/sway/config.preserved-backup"
	applyError := "stale plan; " + strings.Repeat("recovery context ", 120) + "preserved backup: " + recoveryPath
	ops := &fakeDoctorOperations{applyErr: errors.New(applyError)}
	model := newDoctorModel(context.Background(), ops)
	model.busy = ""
	model.plan = &doctor.Plan{ID: "sway.integration", Changes: []doctor.FileChange{{Path: "/test/config", Preview: preview.String()}}}
	model.noColor = true
	for range 100 {
		model, _ = doctorUpdate(t, model, terminalManageKey("j"))
	}
	if !strings.Contains(model.View().Content, "managed line 89") {
		t.Fatal("end of preview inaccessible")
	}
	model, command := doctorUpdate(t, model, terminalManageKey("y"))
	model, _ = doctorUpdate(t, model, command())
	if model.busy != "" || !strings.Contains(strings.Join(model.feedback, "\n"), "stale plan") || len(model.feedback) == 0 {
		t.Fatal("failure not shown")
	}
	if strings.Contains(ansi.Strip(model.View().Content), recoveryPath) {
		t.Fatal("long failure unexpectedly fit without scrolling")
	}
	for range 100 {
		model, _ = doctorUpdate(t, model, terminalManageKey("j"))
	}
	if !strings.Contains(ansi.Strip(model.View().Content), recoveryPath) {
		t.Fatal("end of repair failure details is not scrollable")
	}
	model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: 20, Height: 8})
	if !strings.Contains(model.View().Content, "Resize") {
		t.Fatal("tiny terminal missing fallback")
	}
}
