package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marang/sway-session/internal/doctor"
)

// Feed the actual public diagnostic report into the TUI: a synthetic one-line
// check cannot prove that ordinary multi-file evidence remains reachable.
func TestDoctorWorkstationReportRemainsInspectableInTUI(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "complete"
		if partial {
			name = "partially checked"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
				dir := filepath.Join(root, key)
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv(key, dir)
			}
			t.Setenv("SWAYSOCK", "")
			t.Setenv("I3SOCK", "")
			t.Setenv("HERDR_CONFIG_PATH", filepath.Join(root, "absent-herdr-config"))
			configDir := filepath.Join(root, "workstation")
			if err := os.CopyFS(configDir, os.DirFS("../../internal/doctor/testdata/workstation")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(configDir, "config")
			if partial {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(content, []byte("bindcode Mod4+36 exec foot\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			service := doctor.New(doctor.Options{SwayConfigPath: path})
			model := newDoctorModel(context.Background(), service)
			model.noColor = true
			model, _ = doctorUpdate(t, model, model.Init()())
			model, _ = doctorUpdate(t, model, terminalManageKey("/"))
			model, _ = doctorUpdate(t, model, terminalManageKey("sway.integration"))
			model, _ = doctorUpdate(t, model, terminalManageKey("enter"))
			selected, ok := model.selected()
			wantStatus := doctor.OK
			if partial {
				wantStatus = doctor.Warning
			}
			if !ok || selected.ID != "sway.integration" || selected.Status != wantStatus || selected.FixID != "" {
				t.Fatalf("unexpected real workstation report: %+v", selected)
			}
			for _, size := range [][2]int{{80, 24}, {48, 16}, {120, 30}} {
				model, _ = doctorUpdate(t, model, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				details := model.detailLines()
				var pages strings.Builder
				for page := 0; page <= len(details); page++ {
					view := ansi.Strip(model.View().Content)
					lines := strings.Split(view, "\n")
					if len(lines) > size[1] {
						t.Fatalf("report exceeds %d rows: %d", size[1], len(lines))
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > size[0] {
							t.Fatalf("report exceeds %d columns: %q", size[0], line)
						}
					}
					if strings.Contains(view, "[f]") || !strings.Contains(view, "[q] Quit") {
						t.Fatalf("unsafe or missing action in report: %s", view)
					}
					pages.WriteString(view)
					previous := model.offset
					model, _ = doctorUpdate(t, model, terminalManageKey("pgdown"))
					if model.offset == previous {
						break
					}
				}
				for _, detail := range details {
					if detail = strings.TrimSpace(detail); detail != "" && !strings.Contains(pages.String(), detail) {
						t.Errorf("detail is not reachable at %dx%d: %q", size[0], size[1], detail)
					}
				}
				for model.offset > 0 {
					want := max(0, model.offset-model.detailHeight())
					model, _ = doctorUpdate(t, model, terminalManageKey("pgup"))
					if model.offset != want {
						t.Fatalf("PgUp skipped visible details: offset=%d want=%d", model.offset, want)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(configDir, "50-sway-session-doctor.conf")); !os.IsNotExist(err) {
				t.Fatalf("viewing the report created a repair snippet: %v", err)
			}
		})
	}
}
