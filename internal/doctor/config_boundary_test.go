package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairRefusesAmbiguousShortcutSyntax(t *testing.T) {
	for name, config := range map[string]string{
		"block":                        "bindsym {\n $mod+Return exec foot\n}\n",
		"mode":                         "mode resize {\n bindsym $mod+Return exec foot\n}\n",
		"mode selection":               "mode resize\n",
		"literal modifier":             "bindsym Mod4+Return exec foot\n",
		"reordered modifiers":          "bindsym Shift+Mod4+Return exec foot\n",
		"variable alias":               "set $terminalkey Mod4+Return\nbindsym $terminalkey exec foot\n",
		"unknown option":               "bindsym --unknown $mod+Return exec foot\n",
		"keycode":                      "bindcode Mod4+36 exec foot\n",
		"unbind":                       "unbindsym $mod+Return\n",
		"unknown return modifier":      "bindsym Group1+Mod4+Return exec foot\n",
		"uppercase binding":            "BINDSYM Mod4+Return exec foot\n",
		"uppercase mode":               "MODE resize\n",
		"variable command":             "set $shortcut bindsym $mod+Return exec foot\n$shortcut\n",
		"variable command prefix":      "set $binding bindsym\n$binding Mod4+Return exec foot\n",
		"variable startup executable":  "set $session sway-session\nexec $session daemon\n",
		"variable startup argument":    "set $action daemon\nexec sway-session $action\n",
		"variable uppercase startup":   "set $session sway-session\nEXEC --no-startup-id $session daemon\n",
		"nested key alias":             "set $base Mod4+Return\nset $key $base\nbindsym $key exec foot\n",
		"quoted startup":               "exec \"sway-session daemon\"\n",
		"env wrapped startup":          "exec env sway-session daemon\n",
		"shell wrapped startup":        "exec sh -c 'sway-session daemon'\n",
		"unknown exec option":          "exec --unknown sway-session daemon\n",
		"unknown binding exec option":  "bindsym $mod+Return exec --unknown sway-session terminal --new\n",
		"repeated exec option":         "exec --no-startup-id --no-startup-id sway-session daemon\n",
		"repeated binding exec option": "bindsym $mod+Return exec --no-startup-id --no-startup-id sway-session terminal --new\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "set $mod Mod4\n"+config)
			service := New(Options{SwayConfigPath: root})
			if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
				t.Fatal("ambiguous or conflicting shortcut offered repair")
			}
			if check := inspectSwayConfig(context.Background(), service.options)[0]; check.FixID != "" {
				t.Fatalf("inspection offered unsafe repair: %+v", check)
			}
		})
	}
}

func TestInspectRecognizesUppercaseSwayKeywords(t *testing.T) {
	config := strings.NewReplacer("set ", "SET ", "exec ", "EXEC ", "bindsym ", "BINDSYM ").Replace(healthySwayConfig())
	root := writeSwayConfig(t, config)
	if check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]; check.Status != OK {
		t.Fatalf("case-insensitive Sway keywords were not recognized: %+v", check)
	}
}

func TestInspectRecognizesEquivalentDefaultBindings(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n"+
		"exec sway-session daemon\nexec sway-session restore\n"+
		"bindsym Mod4+Return exec sway-session terminal --new\n"+
		"bindsym Shift+Mod4+Return exec sway-session terminal --ephemeral\n")
	if check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]; check.Status != OK {
		t.Fatalf("equivalent bindings were not recognized: %+v", check)
	}
}

func TestInspectIncludesEachFileOnce(t *testing.T) {
	root := writeSwayConfig(t, "include session.conf\ninclude *.conf\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "session.conf"), []byte(healthySwayConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if check.Status != OK || !containsEvidence(check.Evidence, "followed 2 safe") {
		t.Fatalf("same include was counted more than once: %+v", check)
	}
}

func TestRepairRequiresModifierAtFirstSnippetInclusion(t *testing.T) {
	for name, content := range map[string]string{
		"undefined":                          "# valid configuration without modifier variable\n",
		"explicit include before definition": "include 50-sway-session-doctor.conf\nset $mod Mod4\n",
		"glob before definition":             "include *.conf\nset $mod Mod4\n",
		"invalid modifier":                   "set $mod banana\n",
		"ambiguous shifted modifier":         "set $mod Mod4+Shift\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, content)
			if err := os.WriteFile(filepath.Join(filepath.Dir(root), "other.conf"), []byte("# existing file\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			service := New(Options{SwayConfigPath: root})
			if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
				t.Fatal("repair would insert invalid default shortcuts")
			}
			if check := inspectSwayConfig(context.Background(), service.options)[0]; check.FixID != "" {
				t.Fatalf("inspection offered invalid repair: %+v", check)
			}
		})
	}
}

func TestRepairConvergesWhenSnippetAlsoMatchesGlob(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\ninclude *.conf\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "other.conf"), []byte("# existing file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if check := inspectSwayConfig(context.Background(), service.options)[0]; check.Status != OK {
		t.Fatalf("repair did not converge: %+v", check)
	}
}
