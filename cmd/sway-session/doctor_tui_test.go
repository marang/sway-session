package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
)

func doctorUpdate(t *testing.T, model doctorModel, msg tea.Msg) (doctorModel, tea.Cmd) {
	t.Helper()
	updated, command := model.Update(msg)
	return updated.(doctorModel), command
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

func TestDoctorTUIRepairRequiresPreviewAndConfirmation(t *testing.T) {
	ops := &fakeDoctorOperations{report: doctor.Report{Checks: []doctor.Check{{ID: "sway.startup", Title: "Startup", Status: doctor.Warning, FixID: "sway.integration"}}}, plan: doctor.Plan{ID: "sway.integration", Summary: "Add missing integration", Changes: []doctor.FileChange{{Path: "/test/config", Preview: "+ include managed.conf"}}}}
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
	if model.busy != "" || model.plan != nil {
		t.Fatal("repair did not converge to report")
	}
}

func TestDoctorTUIPreviewScrollAndFailure(t *testing.T) {
	var preview strings.Builder
	for i := range 90 {
		fmt.Fprintf(&preview, "+ managed line %d\n", i)
	}
	ops := &fakeDoctorOperations{applyErr: errors.New("stale plan; no changes applied")}
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
	if model.busy != "" || !strings.Contains(model.message, "stale plan") {
		t.Fatal("failure not shown")
	}
	model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: 20, Height: 8})
	if !strings.Contains(model.View().Content, "Resize") {
		t.Fatal("tiny terminal missing fallback")
	}
}
