package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
	"github.com/marang/sway-session/internal/swayipc"
)

const doctorAcceptanceHeader = "# Managed by sway-session doctor. Manual edits disable automatic repair.\n# sway-session-doctor-format: 1\n"

func doctorAcceptanceSnippet(executable string, shortcuts bool) string {
	text := doctorAcceptanceHeader + "exec --no-startup-id " + executable + " daemon\nexec --no-startup-id " + executable + " restore\n"
	if shortcuts {
		text += "bindsym $mod+Return exec --no-startup-id " + executable + " terminal --new\nbindsym $mod+Shift+Return exec --no-startup-id " + executable + " terminal --ephemeral\n"
	}
	return text
}

func doctorAcceptanceIntegration(t *testing.T, report doctor.Report) doctor.Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == "sway.integration" {
			return check
		}
	}
	t.Fatal("integration check missing")
	return doctor.Check{}
}

func doctorAcceptanceEnvironment(t *testing.T, h *restoreCleanupHeadless) {
	t.Helper()
	for _, item := range h.env {
		key, value, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "XDG_") {
			t.Setenv(key, value)
		}
	}
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(h.root, "absent-herdr-config"))
	t.Setenv("SWAYSOCK", "")
	t.Setenv("I3SOCK", "")
}

// Only disposable sources are loaded. All startup declarations target our inert
// program. GET_CONFIG verifies source selection, never effective key bindings.
func TestDoctorConfigurationHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("set SWAY_SESSION_HEADLESS_INTEGRATION=1 for private Sway configuration acceptance")
	}
	if _, err := exec.LookPath("sway"); err != nil {
		t.Skipf("private Sway unavailable: %v", err)
	}
	for _, name := range []string{"startup-only", "default-shortcuts", "repeated-include", "indirect-include", "new-adoption", "legacy-partial", "manual-edit"} {
		t.Run(name, func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			doctorAcceptanceEnvironment(t, h)
			executable := filepath.Join(h.root, "sway-session")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(h.root, "config", "sway.conf")
			snippet := filepath.Join(filepath.Dir(config), "50-sway-session-doctor.conf")
			content := "xwayland disable\noutput HEADLESS-1 mode 1280x720\nworkspace 98 output HEADLESS-1\nworkspace 98\nset $mod Mod4\n"
			// A safe shell-valued wallpaper/idle-shaped command is intentionally
			// irrelevant to our source inspection. Native Sway may interpret it.
			content += "set $wallpaper /usr/share/backgrounds\nexec /usr/bin/true \"$(/usr/bin/true)\" $wallpaper\n"
			profile := doctorAcceptanceSnippet(executable, name == "default-shortcuts")
			switch name {
			case "new-adoption":
				profile = ""
			case "legacy-partial":
				profile = doctorAcceptanceHeader + "exec --no-startup-id " + executable + " daemon\n"
			case "manual-edit":
				profile += "# manually changed\n"
			}
			if profile != "" {
				if err := os.WriteFile(snippet, []byte(profile), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "new-adoption":
				content += "# include " + snippet + "\nexec /usr/bin/true include " + snippet + "\n"
			case "indirect-include":
				indirect := filepath.Join(filepath.Dir(config), "indirect.conf")
				if err := os.WriteFile(indirect, []byte("include "+snippet+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				content += "include " + indirect + "\n"
			default:
				content += "include \"" + snippet + "\"\n"
				if name == "repeated-include" {
					content += "include \"" + snippet + "\"\n"
				}
			}
			if err := os.WriteFile(config, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			validate := exec.CommandContext(h.ctx, "sway", "-C", "--config", config)
			validate.Env = h.env
			output, err := validate.CombinedOutput()
			if err != nil || strings.Contains(string(output), "Error on line") {
				t.Fatalf("Sway rejected owned fixture %s: %v\n%s", name, err, output)
			}
			h.command("reload")
			var loaded struct {
				Config string `json:"config"`
			}
			message, err := h.client.RequestContext(h.ctx, swayipc.MessageType(9), nil)
			if err != nil || json.Unmarshal(message.Payload, &loaded) != nil || !strings.Contains(loaded.Config, "workspace 98") {
				t.Fatalf("private loaded-source fact missing: %v", err)
			}
			service := doctor.New(doctor.Options{Socket: h.socket, ConfigPath: filepath.Join(h.root, "absent-session-config"), Executable: executable})
			report := service.Check(h.ctx)
			check := doctorAcceptanceIntegration(t, report)
			want, fix := doctor.OK, true
			switch name {
			case "indirect-include":
				want = doctor.Warning
			case "new-adoption":
				want = doctor.Unavailable
			case "legacy-partial":
				want, fix = doctor.Warning, false
			case "manual-edit":
				want, fix = doctor.Unavailable, false
			}
			if check.Status != want || (check.FixID != "") != fix {
				t.Fatalf("source authority mismatch: %+v", check)
			}
			assertDoctorConfigurationPresentation(t, report, service)
			deps := testDependencies(t)
			deps.newDoctor = func(doctor.Options) doctorOperations { return service }
			for _, structured := range []bool{false, true} {
				args := []string{"doctor", "--check"}
				if structured {
					args = append([]string{"--json"}, args...)
				}
				var stdout, stderr bytes.Buffer
				_ = runWith(args, strings.NewReader(""), &stdout, &stderr, deps)
				if structured {
					var result commandResult
					if json.Unmarshal(stdout.Bytes(), &result) != nil || result.Doctor == nil {
						t.Fatalf("structured result: %s %s", stdout.String(), stderr.String())
					}
					public := doctorAcceptanceIntegration(t, *result.Doctor)
					if public.Status != want || (public.FixID != "") != fix {
						t.Fatalf("JSON changed source authority: %+v", public)
					}
					if strings.Contains(stdout.String(), "adoption_required") {
						t.Fatal("private UI selection leaked into public JSON")
					}
				} else if !strings.Contains(stdout.String(), "literal direct include") {
					t.Fatalf("text omitted source evidence: %s", stdout.String())
				}
			}
			before, err := os.ReadFile(config)
			if err != nil || string(before) != content {
				t.Fatalf("inspection wrote main file: %v", err)
			}
			if name == "new-adoption" {
				if _, err := service.Plan(h.ctx, "sway.integration", doctor.RepairOptions{}); err == nil {
					t.Fatal("implicit adoption allowed")
				}
				plan, err := service.Plan(h.ctx, "sway.integration", doctor.RepairOptions{AdoptStandard: true})
				if err != nil || len(plan.Changes) != 2 {
					t.Fatalf("adoption preview: %+v %v", plan, err)
				}
				if _, err := os.Stat(snippet); !os.IsNotExist(err) {
					t.Fatalf("preview wrote snippet: %v", err)
				}
				if err := os.WriteFile(config, append(before, []byte("# concurrent editor\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Apply(h.ctx, plan); err == nil {
					t.Fatal("stale main plan applied")
				}
				if err := os.WriteFile(config, before, 0600); err != nil {
					t.Fatal(err)
				}
				// Exercise the real CLI orchestration, not just the Plan helper.
				var stdout, stderr bytes.Buffer
				code := runWith([]string{"--json", "doctor", "--fix", "sway.integration", "--adopt-standard", "--yes"}, strings.NewReader(""), &stdout, &stderr, deps)
				var result commandResult
				// The deliberately absent explicit session config remains an
				// independent runtime error after the source-file repair succeeds.
				if code != exitOperation || json.Unmarshal(stdout.Bytes(), &result) != nil || result.DoctorFix == nil || result.Doctor == nil {
					t.Fatalf("integrated CLI apply: code=%d %s %s", code, stdout.String(), stderr.String())
				}
				if repaired := doctorAcceptanceIntegration(t, *result.Doctor); repaired.Status != doctor.OK {
					t.Fatalf("runtime error obscured successful integration repair: %+v", repaired)
				}
				if after, err := os.ReadFile(snippet); err != nil || string(after) != doctorAcceptanceSnippet(executable, false) {
					t.Fatalf("new setup not startup-only: %v", err)
				}
				if after, err := os.ReadFile(config); err != nil || !bytes.HasPrefix(after, before) || strings.Count(string(after[len(before):]), "include ") != 1 {
					t.Fatalf("adoption changed foreign configuration: %v", err)
				}
				if len(result.DoctorFix.Backups) != 1 {
					t.Fatalf("original backup absent: %+v", result.DoctorFix)
				}
				backup, err := os.ReadFile(result.DoctorFix.Backups[0])
				info, statErr := os.Stat(result.DoctorFix.Backups[0])
				if err != nil || statErr != nil || !bytes.Equal(backup, before) || info.Mode().Perm() != 0600 {
					t.Fatalf("original private backup invalid: %v %v", err, statErr)
				}
			} else if !fix {
				if _, err := service.Plan(h.ctx, "sway.integration", doctor.RepairOptions{AdoptStandard: true, Shortcuts: doctor.ShortcutsDefault}); err == nil {
					t.Fatal("protected migration allowed automatic repair")
				}
				if after, err := os.ReadFile(snippet); err != nil || string(after) != profile {
					t.Fatalf("protected file changed: %v", err)
				}
			}
			t.Logf("%s: private Sway sources, CLI text/JSON, bounded TUI and repair authority checked", name)
		})
	}
}

// A second isolated compositor loads each actual owned profile at startup.
// The recorded calls prove exec-versus-reload behavior only, not daemon health
// or execution of the optional terminal bindings.
func TestDoctorStandardStartupHeadless(t *testing.T) {
	if os.Getenv("SWAY_SESSION_HEADLESS_INTEGRATION") != "1" {
		t.Skip("opt-in private Sway startup acceptance")
	}
	for _, shortcuts := range []bool{false, true} {
		t.Run(fmt.Sprintf("shortcuts=%v", shortcuts), func(t *testing.T) {
			h := newRestoreCleanupHeadless(t)
			executable := filepath.Join(h.root, "sway-session")
			calls := filepath.Join(h.root, "startup-calls")
			program := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + calls + "'\n"
			if err := os.WriteFile(executable, []byte(program), 0700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(h.root, "config", "startup.conf")
			snippet := filepath.Join(filepath.Dir(config), "50-sway-session-doctor.conf")
			if err := os.WriteFile(snippet, []byte(doctorAcceptanceSnippet(executable, shortcuts)), 0600); err != nil {
				t.Fatal(err)
			}
			content := "xwayland disable\nworkspace 98\nset $mod Mod4\ninclude \"" + snippet + "\"\n"
			if err := os.WriteFile(config, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			process := h.start("sway", "--config", config)
			socket := filepath.Join(h.root, "run", fmt.Sprintf("sway-ipc.%d.%d.sock", os.Getuid(), process.Process.Pid))
			h.until("profile startup calls and private socket", func() bool {
				content, err := os.ReadFile(calls)
				_, socketErr := os.Stat(socket)
				return err == nil && socketErr == nil && len(strings.Fields(string(content))) >= 2
			})
			client := swayipc.NewClient(socket)
			defer client.Close()
			for range 2 {
				message, err := client.RequestContext(h.ctx, swayipc.RunCommand, []byte("reload"))
				if err != nil || swayipc.CheckRunCommandResponse(message) != nil {
					t.Fatalf("private profile reload: %v", err)
				}
			}
			select {
			case <-time.After(250 * time.Millisecond):
			case <-h.ctx.Done():
				t.Fatal(h.ctx.Err())
			}
			contentBytes, err := os.ReadFile(calls)
			observed := strings.Fields(string(contentBytes))
			if err != nil || len(observed) != 2 || strings.Count(string(contentBytes), "daemon\n") != 1 || strings.Count(string(contentBytes), "restore\n") != 1 {
				t.Fatalf("startup/reload calls=%q error=%v", observed, err)
			}
			t.Log("one daemon and one restore declaration executed; two reloads caused no additional calls")
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
