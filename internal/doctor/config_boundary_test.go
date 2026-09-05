package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairRefusesAmbiguousShortcutSyntax(t *testing.T) {
	for name, config := range map[string]string{
		"block":                         "bindsym {\n $mod+Return exec foot\n}\n",
		"mode selection":                "mode resize\n",
		"literal modifier":              "bindsym Mod4+Return exec foot\n",
		"reordered modifiers":           "bindsym Shift+Mod4+Return exec foot\n",
		"variable alias":                "set $terminalkey Mod4+Return\nbindsym $terminalkey exec foot\n",
		"unknown option":                "bindsym --unknown $mod+Return exec foot\n",
		"keycode":                       "bindcode Mod4+36 exec foot\n",
		"unbind":                        "unbindsym $mod+Return\n",
		"unknown return modifier":       "bindsym Group1+Mod4+Return exec foot\n",
		"uppercase binding":             "BINDSYM Mod4+Return exec foot\n",
		"uppercase mode":                "MODE resize\n",
		"variable command":              "set $shortcut bindsym $mod+Return exec foot\n$shortcut\n",
		"variable command prefix":       "set $binding bindsym\n$binding Mod4+Return exec foot\n",
		"variable startup executable":   "set $session sway-session\nexec $session daemon\n",
		"variable startup argument":     "set $action daemon\nexec sway-session $action\n",
		"variable uppercase startup":    "set $session sway-session\nEXEC --no-startup-id $session daemon\n",
		"nested key alias":              "set $base Mod4+Return\nset $key $base\nbindsym $key exec foot\n",
		"quoted startup":                "exec \"sway-session daemon\"\n",
		"env wrapped startup":           "exec env sway-session daemon\n",
		"env option wrapped startup":    "exec env -i sway-session daemon\n",
		"env chdir wrapped startup":     "exec env -C /tmp sway-session daemon\n",
		"env split wrapped startup":     "exec env -S 'sway-session daemon'\n",
		"env split option startup":      "exec env -S '-i sway-session daemon'\n",
		"env split assignment startup":  "exec env -S 'FOO=bar sway-session daemon'\n",
		"shell wrapped startup":         "exec sh -c 'sway-session daemon'\n",
		"shell exec wrapped startup":    "exec sh -c 'exec sway-session daemon'\n",
		"shell command wrapped startup": "exec sh -c 'command sway-session daemon'\n",
		"shell env wrapped startup":     "exec sh -c 'env sway-session daemon'\n",
		"shell chained startup":         "exec sh -c 'sway-session; daemon'\n",
		"unknown exec option":           "exec --unknown sway-session daemon\n",
		"unknown binding exec option":   "bindsym $mod+Return exec --unknown sway-session terminal --new\n",
		"repeated exec option":          "exec --no-startup-id --no-startup-id sway-session daemon\n",
		"repeated binding exec option":  "bindsym $mod+Return exec --no-startup-id --no-startup-id sway-session terminal --new\n",
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

func TestInspectDefaultModeBlockCanConflictWithDefaultShortcut(t *testing.T) {
	root := writeSwayConfig(t, healthySwayConfig()+
		"mode \"default\" {\n bindsym $mod+Return exec foot\n}\n")
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if check.Status != Warning || check.FixID != "" || !evidenceLineContains(check.Evidence, "persistent-terminal shortcut", "conflicting") {
		t.Fatalf("default mode binding was not classified as a conflict: %+v", check)
	}
}

func TestInspectDoesNotCountModeBodyExecAsStartup(t *testing.T) {
	root := writeSwayConfig(t,
		"set $mod Mod4\n"+
			"mode \"default\" {\n exec sway-session daemon\n exec sway-session restore\n}\n"+
			"bindsym $mod+Return exec sway-session terminal --new\n"+
			"bindsym $mod+Shift+Return exec sway-session terminal --ephemeral\n")
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if !evidenceLineContains(check.Evidence, "daemon startup", "missing") ||
		!evidenceLineContains(check.Evidence, "restore startup", "missing") {
		t.Fatalf("invalid mode subcommands were counted as top-level startups: %+v", check)
	}
}

func TestInspectModeSetUpdatesVariablesForFollowingDefaultBindings(t *testing.T) {
	root := writeSwayConfig(t,
		"mode \"resize\" {\n set $mod Mod4\n bindsym Return mode default\n}\n"+
			strings.TrimPrefix(healthySwayConfig(), "set $mod Mod4\n"))
	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if check.Status != OK {
		t.Fatalf("mode set subcommand did not update global variables: %+v", check)
	}
}

func TestInspectCapsRepeatedDeclarationEvidence(t *testing.T) {
	var config strings.Builder
	config.WriteString("set $mod Mod4\n")
	for range maxSwayEvidenceItems + 5 {
		config.WriteString("exec --no-startup-id /usr/bin/sway-session daemon\n")
	}
	config.WriteString("exec --no-startup-id /usr/bin/sway-session restore\n")
	config.WriteString("bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new\n")
	config.WriteString("bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral\n")
	root := writeSwayConfig(t, config.String())

	check := inspectSwayConfig(context.Background(), Options{SwayConfigPath: root})[0]
	if check.Status != Warning || !containsEvidence(check.Evidence, "additional declarations omitted") {
		t.Fatalf("repeated declarations were not summarized: %+v", check)
	}
	daemonEvidence := 0
	for _, evidence := range check.Evidence {
		if strings.Contains(evidence, "daemon startup") {
			daemonEvidence++
		}
	}
	if daemonEvidence > maxSwayEvidenceItems+2 {
		t.Fatalf("daemon evidence is unbounded: %d lines in %+v", daemonEvidence, check.Evidence)
	}
}

func TestRepairRefusesUnsupportedIncludeBlock(t *testing.T) {
	root := writeSwayConfig(t, healthySwayConfig()+"include {\n hidden.conf\n}\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "hidden.conf"),
		[]byte("bindsym $mod+Return exec foot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(context.Background(), service.options)[0]
	if check.Status != Warning || !strings.Contains(check.Detail, "partially checked") || check.FixID != "" {
		t.Fatalf("unsupported include block did not preserve uncertainty: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
		t.Fatal("unsupported include block allowed automatic repair")
	}
}

func TestScannerLatchesIncludeGraphByteLimit(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	paths := []string{
		filepath.Join(directory, "a.conf"),
		filepath.Join(directory, "b.conf"),
		filepath.Join(directory, "c.conf"),
		filepath.Join(directory, "d.conf"),
	}
	var rootContent strings.Builder
	for _, path := range paths {
		rootContent.WriteString("include " + path + "\n")
	}
	edits := []fileEdit{{path: root, newContent: []byte(rootContent.String())}}
	large := bytes.Repeat([]byte("#\n"), maxSwayConfigBytes/2)
	for _, path := range paths {
		edits = append(edits, fileEdit{path: path, newContent: large})
	}
	for index := range 80 {
		path := filepath.Join(directory, fmt.Sprintf("extra-%d.conf", index))
		rootContent.WriteString("include " + path + "\n")
		edits = append(edits, fileEdit{path: path, newContent: []byte("# extra\n")})
	}
	edits[0].newContent = []byte(rootContent.String())

	analysis, err := analyzeSwayConfigWithEdits(context.Background(), Options{SwayConfigPath: root}, edits)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.observed) > 5 {
		t.Fatalf("scanner kept reading after byte exhaustion: observed %d files", len(analysis.observed))
	}
	if len(analysis.includes) > len(paths) {
		t.Fatalf("scanner kept expanding includes after byte exhaustion: recorded %d paths", len(analysis.includes))
	}
}

func TestScannerBoundsBlockNesting(t *testing.T) {
	var config strings.Builder
	for range maxSwayBlockDepth + 1 {
		config.WriteString("output * {\n")
	}
	root := writeSwayConfig(t, config.String())

	analysis, err := analyzeSwayConfig(context.Background(), Options{SwayConfigPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.unsupported == nil || !strings.Contains(analysis.unsupported.Error(), "block nesting") {
		t.Fatalf("deep block graph was not bounded: %+v", analysis)
	}
}

func TestRepairRefusesVariableDerivedModeSemantics(t *testing.T) {
	for name, suffix := range map[string]string{
		"mode name mutation":    "set $name resize\nmode $name {\n set $$name default\n bindsym $mod+Return exec foot\n}\n",
		"mode header injection": "set $name default bindsym Mod4+Return exec foot\nmode $name {\n --title hello\n}\n",
		"mode body command":     "set $cmd set $$mod\nmode resize {\n $cmd Mod1\n}\nbindsym Mod1+Return exec foot\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, "set $mod Mod4\n"+suffix)
			service := New(Options{SwayConfigPath: root})
			check := inspectSwayConfig(context.Background(), service.options)[0]
			if check.FixID != "" || !strings.Contains(check.Hint, "Automatic repair is unavailable") {
				t.Fatalf("variable-derived mode semantics offered repair: %+v", check)
			}
			if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
				t.Fatal("variable-derived mode semantics allowed automatic repair")
			}
		})
	}
}

func TestRepairRefusesCommandCombinedWithModeClosingBrace(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\nmode default {\n bindsym $mod+Return exec foot }\n")
	service := New(Options{SwayConfigPath: root})
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
		t.Fatal("command on mode closing line allowed automatic repair")
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
