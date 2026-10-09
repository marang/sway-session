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

func TestDoctorDoesNotUseIncludeEvidenceAfterInspectionBound(t *testing.T) {
	for name, source := range map[string]string{
		"block opener":           "exec /usr/bin/true " + strings.Repeat(" ", maxSwayConfigLine) + "{\ninclude " + doctorSnippetName + "\n}\n",
		"continued block opener": "exec /usr/bin/true \\\n" + strings.Repeat(" ", maxSwayConfigLine) + "{\ninclude " + doctorSnippetName + "\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, source)
			snippet := filepath.Join(filepath.Dir(root), doctorSnippetName)
			original := renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon, integrationRestore})
			if err := os.WriteFile(snippet, original, 0o600); err != nil {
				t.Fatal(err)
			}
			service := New(Options{SwayConfigPath: root})
			check := inspectSwayConfig(t.Context(), service.options)[0]
			if check.Status == OK || check.FixID != "" || containsEvidence(check.Evidence, "literal direct include: present") {
				t.Fatalf("uninspected block produced direct-include evidence or repair authority: %+v", check)
			}
			for _, adopt := range []bool{false, true} {
				_, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: adopt, Shortcuts: ShortcutsDefault})
				if err == nil {
					t.Fatalf("uninspected block allowed profile change with adoption=%t", adopt)
				}
			}
			if after, err := os.ReadFile(root); err != nil || string(after) != source {
				t.Fatalf("rejected plan changed main: %q, %v", after, err)
			}
			if after, err := os.ReadFile(snippet); err != nil || string(after) != string(original) {
				t.Fatalf("rejected plan changed standard file: %q, %v", after, err)
			}
		})
	}
}

func TestDoctorIncludeBlockRequiresExplicitAdoption(t *testing.T) {
	for _, gap := range []string{"", "\n \t\n"} {
		root := writeStandardSwayConfig(t, ShortcutsNone, true)
		source := "include " + doctorSnippetName + "\n" + gap + "{\n}\n"
		if err := os.WriteFile(root, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		service := New(Options{SwayConfigPath: root})
		check := inspectSwayConfig(t.Context(), service.options)[0]
		if check.Status != Warning || !check.AdoptionRequired {
			t.Fatalf("block header granted direct-include authority: %+v", check)
		}
		if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: ShortcutsDefault}); err == nil || !strings.Contains(err.Error(), "explicit adoption") {
			t.Fatalf("block header allowed profile change without adoption: %v", err)
		}
		plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault})
		if err != nil || len(plan.Changes) != 2 {
			t.Fatalf("explicit adoption did not append a standalone include and change profile: %+v %v", plan, err)
		}
		if _, err := service.Apply(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		if after, err := os.ReadFile(root); err != nil || !strings.HasPrefix(string(after), source) {
			t.Fatalf("adoption changed existing block: %q %v", after, err)
		}
		if check := inspectSwayConfig(t.Context(), service.options)[0]; check.Status != OK || check.AdoptionRequired {
			t.Fatalf("explicit standalone include did not converge: %+v", check)
		}
	}
}

func TestDoctorEscapedSpaceBracePreservesRealInclude(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsNone, true)
	source := "exec /usr/bin/printf \\ {\ninclude " + doctorSnippetName + "\n"
	if err := os.WriteFile(root, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	if check := inspectSwayConfig(t.Context(), service.options)[0]; check.Status != OK || check.AdoptionRequired {
		t.Fatalf("opaque exec payload hid real direct include: %+v", check)
	}
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: ShortcutsDefault})
	if err != nil || len(plan.Changes) != 1 {
		t.Fatalf("existing integration could not switch just its profile: %+v %v", plan, err)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(root); err != nil || string(after) != source {
		t.Fatalf("profile switch changed foreign exec: %q %v", after, err)
	}
}

func TestDoctorUnicodeFilenameDoesNotGrantDirectIncludeAuthority(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsNone, true)
	foreign := filepath.Join(filepath.Dir(root), doctorSnippetName+"\u00a0")
	originalForeign := "# distinct foreign file\n"
	if err := os.WriteFile(foreign, []byte(originalForeign), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "include " + doctorSnippetName + "\u00a0\n"
	if err := os.WriteFile(root, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(t.Context(), service.options)[0]
	if check.Status != Warning || !check.AdoptionRequired || containsEvidence(check.Evidence, "literal direct include: present") {
		t.Fatalf("foreign Unicode filename granted managed include authority: %+v", check)
	}
	if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: ShortcutsDefault}); err == nil || !strings.Contains(err.Error(), "explicit adoption") {
		t.Fatalf("foreign Unicode filename allowed profile change without adoption: %v", err)
	}
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault})
	if err != nil || len(plan.Changes) != 2 {
		t.Fatalf("adoption must add a real include and change the profile: %+v %v", plan, err)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(root); err != nil || !strings.HasPrefix(string(after), source) {
		t.Fatalf("adoption changed original Unicode include: %q %v", after, err)
	}
	if after, err := os.ReadFile(foreign); err != nil || string(after) != originalForeign {
		t.Fatalf("adoption touched foreign Unicode file: %q %v", after, err)
	}
	if check := inspectSwayConfig(t.Context(), service.options)[0]; check.Status != OK || check.AdoptionRequired {
		t.Fatalf("standalone managed include failed to converge: %+v", check)
	}
}
