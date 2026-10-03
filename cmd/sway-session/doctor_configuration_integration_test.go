package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
	"github.com/marang/sway-session/internal/swayipc"
)

// Sway validates and loads only disposable configuration. Startup declarations
// target an inert, owned executable; this test never launches the user daemon.
func TestDoctorConfigurationHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway configuration acceptance")
	}
	if _, err := exec.LookPath("sway"); err != nil {
		t.Skipf("private Sway unavailable: %v", err)
	}
	for _, name := range []string{"complete", "partial", "conflicting", "missing"} {
		t.Run(name, func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			for _, item := range h.env {
				key, value, _ := strings.Cut(item, "=")
				if strings.HasPrefix(key, "XDG_") {
					t.Setenv(key, value)
				}
			}
			t.Setenv("HERDR_CONFIG_PATH", filepath.Join(h.root, "absent-herdr-config"))
			executable := filepath.Join(h.root, "sway-session")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(h.root, "fixture")
			if err := os.CopyFS(fixture, os.DirFS("../../internal/doctor/testdata/workstation")); err != nil {
				t.Fatal(err)
			}
			for _, relative := range []string{"config", "conf.d/10-startup.conf", "conf.d/20-terminals.conf"} {
				path := filepath.Join(fixture, relative)
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				text := strings.ReplaceAll(string(content), "/usr/bin/sway-session", executable)
				text = strings.ReplaceAll(text, "status_command while date; do sleep 1; done", "status_command { /usr/bin/true; } | /usr/bin/cat")
				text = strings.ReplaceAll(text, "exec swayidle", "exec /usr/bin/true")
				if name == "missing" && strings.HasSuffix(relative, "20-terminals.conf") {
					text = strings.Split(text, "bindsym $mod+Shift+Return")[0]
				}
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			config := filepath.Join(h.root, "config", "sway.conf")
			content := "xwayland disable\noutput HEADLESS-1 mode 1280x720\nworkspace 98 output HEADLESS-1\nworkspace 98\ninclude " + filepath.Join(fixture, "config") + "\n"
			switch name {
			case "partial":
				content += "bindcode --no-warn Mod4+36 exec /usr/bin/true\n"
			case "conflicting":
				content += "bindsym --no-warn $mod+Return exec /usr/bin/true\n"
			}
			if err := os.WriteFile(config, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			validate := exec.CommandContext(h.ctx, "sway", "-C", "--config", config)
			validate.Env = h.env
			if output, err := validate.CombinedOutput(); err != nil {
				t.Fatalf("Sway rejected %s fixture: %v\n%s", name, err, output)
			}
			h.command("reload")
			// These IPC facts establish selection, loaded text and current mode.
			// They cannot enumerate evaluated bindings or prove startup success.
			var loaded struct {
				Config string `json:"config"`
			}
			message, err := h.client.RequestContext(h.ctx, swayipc.MessageType(9), nil)
			if err != nil || message.Type != 9 || json.Unmarshal(message.Payload, &loaded) != nil || !strings.Contains(loaded.Config, "workspace 98") {
				t.Fatalf("missing private GET_CONFIG fact: %v", err)
			}
			var mode struct {
				Name string `json:"name"`
			}
			message, err = h.client.RequestContext(h.ctx, swayipc.MessageType(12), nil)
			if err != nil || message.Type != 12 || json.Unmarshal(message.Payload, &mode) != nil || mode.Name != "default" {
				t.Fatalf("unexpected actual binding mode: %+v %v", mode, err)
			}
			service := doctor.New(doctor.Options{Socket: h.socket, ConfigPath: filepath.Join(h.root, "absent-session-config"), Executable: executable})
			report := service.Check(h.ctx)
			var check doctor.Check
			for _, candidate := range report.Checks {
				if candidate.ID == "sway.integration" {
					check = candidate
				}
			}
			want := doctor.OK
			if name != "complete" {
				want = doctor.Warning
			}
			if check.ID == "" || check.Status != want || (check.FixID != "") != (name == "missing") {
				t.Fatalf("unexpected public integration report: %+v", check)
			}
			for _, label := range []string{"daemon startup", "restore startup", "persistent-terminal shortcut", "ephemeral-terminal shortcut"} {
				if !strings.Contains(strings.Join(check.Evidence, "\n"), label) {
					t.Fatalf("missing requirement %q", label)
				}
			}
			assertDoctorConfigurationPresentation(t, report, service)
			// Short and structured CLI output use the same public service.
			var short bytes.Buffer
			if err := writeDoctorResult(&short, commandResult{Doctor: &report}); err != nil || !strings.Contains(short.String(), "sway.integration") {
				t.Fatalf("short report: %v", err)
			}
			deps := testDependencies(t)
			deps.newDoctor = func(doctor.Options) doctorOperations { return service }
			var stdout, stderr bytes.Buffer
			_ = runWith([]string{"--json", "doctor", "--check"}, strings.NewReader(""), &stdout, &stderr, deps)
			var result commandResult
			if json.Unmarshal(stdout.Bytes(), &result) != nil || result.Doctor == nil {
				t.Fatalf("structured report unavailable: %s %s", stdout.String(), stderr.String())
			}
			found := false
			for _, candidate := range result.Doctor.Checks {
				if candidate.ID == "sway.integration" {
					found = true
					if candidate.Status != want || (candidate.FixID != "") != (name == "missing") {
						t.Fatalf("structured CLI changed integration authority: %+v", candidate)
					}
				}
			}
			if !found {
				t.Fatal("structured CLI omitted the integration check")
			}
			if name == "missing" {
				plan, err := service.Plan(h.ctx, "sway.integration")
				if err != nil || len(plan.Changes) != 2 {
					t.Fatalf("repair preview: %+v %v", plan, err)
				}
				include := filepath.Join(fixture, "conf.d", "20-terminals.conf")
				before, err := os.ReadFile(include)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(include, append(before, []byte("# changed after preview\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Apply(h.ctx, plan); err == nil {
					t.Fatal("stale included-file plan applied")
				}
			} else if _, err := service.Plan(h.ctx, "sway.integration"); err == nil {
				t.Fatal("complete/uncertain/conflicting configuration allowed repair")
			}
			if _, err := os.Stat(filepath.Join(h.root, "config", "50-sway-session-doctor.conf")); !os.IsNotExist(err) {
				t.Fatalf("inspection or rejected preview wrote a snippet: %v", err)
			}
			if after, err := os.ReadFile(config); err != nil || string(after) != content {
				t.Fatalf("inspection changed root config: %v", err)
			}
			t.Logf("Sway -C and loaded config/default-mode facts passed; %s static classification, CLI, 80x24 TUI and repair authority checked", name)
		})
	}
}

func assertDoctorConfigurationPresentation(t *testing.T, report doctor.Report, service doctorOperations) {
	t.Helper()
	model := newDoctorModel(t.Context(), service)
	model.noColor = true
	model, _ = doctorUpdate(t, model, doctorCheckedMsg{report: report})
	model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: 80, Height: 24})
	model, _ = doctorUpdate(t, model, terminalManageKey("/"))
	model, _ = doctorUpdate(t, model, terminalManageKey("sway.integration"))
	model, _ = doctorUpdate(t, model, terminalManageKey("enter"))
	var pages strings.Builder
	for range len(model.detailLines()) + 1 {
		view := ansi.Strip(model.View().Content)
		lines := strings.Split(view, "\n")
		if len(lines) > 24 {
			t.Fatalf("doctor exceeds 24 rows: %d", len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > 80 {
				t.Fatalf("doctor exceeds 80 columns: %q", line)
			}
		}
		pages.WriteString(view)
		previous := model.offset
		model, _ = doctorUpdate(t, model, terminalManageKey("pgdown"))
		if previous == model.offset {
			break
		}
	}
	for _, line := range model.detailLines() {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(pages.String(), line) {
			t.Fatalf("unreachable detail at 80x24: %q", line)
		}
	}
}
