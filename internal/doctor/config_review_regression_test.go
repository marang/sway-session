package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorOpaqueCommandsCannotGrantAdoption(t *testing.T) {
	for _, source := range []string{
		"exec true; sway-session daemon; notify-send \"broken\n",
		"exec sh -c '\"sway-session\" daemon'\n",
		"exec env -S 'sway-session daemon'\n",
		"set $msg hello; sway-session daemon\nexec notify-send $msg\n",
		healthySwayConfig(),
	} {
		root := writeSwayConfig(t, source)
		service := New(Options{SwayConfigPath: root})
		check := inspectSwayConfig(t.Context(), service.options)[0]
		if !check.AdoptionRequired {
			t.Fatalf("opaque/foreign commands granted adoption: %+v", check)
		}
		if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{}); err == nil || !strings.Contains(err.Error(), "explicit adoption") {
			t.Fatalf("missing adoption accepted: %v", err)
		}
		after, err := os.ReadFile(root)
		if err != nil || string(after) != source {
			t.Fatalf("check or rejected preview changed root: %q, %v", after, err)
		}
	}
}

func TestDoctorLegacyPartialProfilesRequireManualMigration(t *testing.T) {
	// Every previously generated subset except the two complete profiles is
	// protected, including the old header-only empty profile.
	for mask := 0; mask < 1<<len(integrationOrder); mask++ {
		if mask == 3 || mask == 15 {
			continue
		}
		kinds := []integrationKind{}
		for index, kind := range integrationOrder {
			if mask&(1<<index) != 0 {
				kinds = append(kinds, kind)
			}
		}
		root := writeSwayConfig(t, "include "+doctorSnippetName+"\n")
		snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
		original := renderManagedSnippet("/usr/bin/sway-session", kinds)
		if err := os.WriteFile(snippet, original, 0o600); err != nil {
			t.Fatal(err)
		}
		service := New(Options{SwayConfigPath: root})
		check := inspectSwayConfig(t.Context(), service.options)[0]
		if check.Status != Warning || check.FixID != "" || !containsEvidence(check.Evidence, "legacy partial") {
			t.Fatalf("partial mask %d offered repair: %+v", mask, check)
		}
		if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault}); err == nil || !strings.Contains(err.Error(), "manual migration") {
			t.Fatalf("partial mask %d accepted adoption: %v", mask, err)
		}
		if after, err := os.ReadFile(snippet); err != nil || string(after) != string(original) {
			t.Fatalf("legacy source changed: %q, %v", after, err)
		}
	}
}
