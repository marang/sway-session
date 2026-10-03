package doctor

import (
	"os"
	"strings"
	"testing"
)

func TestDoctorMalformedLaterStartupCannotAuthorizeRepair(t *testing.T) {
	for name, content := range map[string]string{
		"chain":        "exec true; sway-session daemon; notify-send \"broken\n",
		"shell chain":  "exec true; sh -c 'sway-session daemon\n",
		"prefix block": "exec {\n true; sway-session daemon; notify-send \"broken\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			content = "set $mod Mod4\n" + content
			root := writeSwayConfig(t, content)
			service := New(Options{SwayConfigPath: root})
			check := inspectSwayConfig(t.Context(), service.options)[0]
			if check.FixID != "" || !evidenceLineContains(check.Evidence, "daemon startup", "could not be fully checked") {
				t.Fatalf("malformed later startup gained repair authority: %+v", check)
			}
			if _, err := service.Plan(t.Context(), swayIntegrationFixID); err == nil {
				t.Fatal("unresolved later executable authorized repair")
			}
			if after, err := os.ReadFile(root); err != nil || string(after) != content {
				t.Fatalf("inspection changed configuration: %v", err)
			}
		})
	}
}

func TestDoctorUnrelatedBarShellBracesPreserveIndependentFacts(t *testing.T) {
	bar := "bar {\n status_command { date; } | cat\n}\n"
	for _, content := range []string{bar + healthySwayConfig(), healthySwayConfig() + bar} {
		root := writeSwayConfig(t, content)
		check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
		if check.Status != OK || strings.Contains(strings.Join(check.Evidence, "\n"), "could not be fully checked") {
			t.Fatalf("unrelated bar shell payload lost integration facts: %+v", check)
		}
	}
}

func TestDoctorStartupArgumentExpansionCannotAuthorizeRepair(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\nset $msg hello; sway-session daemon\nexec notify-send $msg\n")
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(t.Context(), service.options)[0]
	if check.FixID != "" || !evidenceLineContains(check.Evidence, "daemon startup", "could not be fully checked") {
		t.Fatalf("argument expansion lost a potential later executable: %+v", check)
	}
	if _, err := service.Plan(t.Context(), swayIntegrationFixID); err == nil {
		t.Fatal("potential expanded startup authorized repair")
	}
}
