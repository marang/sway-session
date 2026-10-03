package doctor

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorQuotedExecutableInShellPayloadBlocksRepair(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\nexec sh -c '\"sway-session\" daemon'\n")
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(t.Context(), service.options)[0]
	if check.FixID != "" || !evidenceLineContains(check.Evidence, "daemon startup", "could not be fully checked") {
		t.Fatalf("quoted executable escaped startup uncertainty: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID); err == nil {
		t.Fatal("quoted indirect startup allowed automatic repair")
	}
}

func classifySwaySource(t *testing.T, content string, seeds map[string]string) swayConfigEvidence {
	t.Helper()
	parser := newSwayConfigClassifier(seeds).source("/fixture/config")
	evidence := newSwayConfigEvidence()
	if err := forEachSwayLogicalLine([]byte(content), func(line swayLogicalLine) bool {
		evidence.record(parser.line(line))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	evidence.record(parser.finish())
	return evidence
}

func TestSwayClassifierUnrelatedScopedParseErrors(t *testing.T) {
	for name, unrelated := range map[string]string{
		"input":             "input * {\n xkb_layout \"broken\n}\n",
		"output":            "output * {\n mode \"broken\n}\n",
		"for window":        "for_window [app_id=\"example\"] {\n floating \"broken\n}\n",
		"media binding":     "bindsym --locked {\n XF86AudioRaiseVolume exec printf \"broken\n}\n",
		"startup block":     "exec {\n notify-send \"broken\n}\n",
		"unrelated startup": "exec notify-send \"broken\n",
	} {
		t.Run(name, func(t *testing.T) {
			evidence := classifySwaySource(t, unrelated+healthySwayConfig(), nil)
			if evidence.unsupported != nil {
				t.Fatalf("unrelated scoped error tainted integration: %v", evidence.unsupported)
			}
			for _, kind := range integrationOrder {
				if evidence.matchingCounts[kind] != 1 || evidence.uncertaintyCounts[kind] != 0 {
					t.Fatalf("lost independent %s evidence: %+v", integrationLabel(kind), evidence)
				}
			}
		})
	}
}

func TestSwayClassifierRelevantParseErrorsBlockRepair(t *testing.T) {
	for _, extra := range []string{
		"exec sh -c '\"sway-session\" daemon\n",
		"bindsym $mod+Return exec \"broken\n",
		"for_window [app_id=\"example\"] {\n exec $unknown\n}\n",
		"input * {\n xkb_layout us } include hidden.conf\n",
		"include \"broken\n",
	} {
		evidence := classifySwaySource(t, healthySwayConfig()+extra, nil)
		if evidence.unsupported == nil {
			t.Fatalf("relevant or structural uncertainty was ignored: %q", extra)
		}
		if _, err := repairableMissing(swayConfigAnalysis{swayConfigEvidence: evidence}); err == nil {
			t.Fatalf("uncertain configuration allowed repair: %q", extra)
		}
	}
}

func TestSwayClassifierOrderedSetAndIncludeFacts(t *testing.T) {
	seeds := map[string]string{"$HOME": "/injected/home"}
	classifier := newSwayConfigClassifier(seeds)
	parent := classifier.source("/fixture/config")
	set := parent.line(swayLogicalLine{text: "set $parts ~/parts", start: 1})
	if set.set == nil || set.set.name != "$parts" || set.set.value != "~/parts" {
		t.Fatalf("missing assignment directive: %+v", set)
	}
	include := parent.line(swayLogicalLine{text: "include $parts/*.conf", start: 2})
	if len(include.includes) != 1 || include.includes[0] != "/injected/home/parts/*.conf" {
		t.Fatalf("include did not use injected ordered assignment: %+v", include)
	}
	child := classifier.source("/fixture/parts/session.conf")
	child.line(swayLogicalLine{text: "set $mod Mod4", start: 1})
	facts := parent.line(swayLogicalLine{text: "bindsym $mod+Return exec sway-session terminal --new", start: 3})
	if len(facts.occurrences) != 1 || !facts.occurrences[0].correct || facts.occurrences[0].where.line != 3 {
		t.Fatalf("included assignment did not affect following parent declaration: %+v", facts)
	}
	if _, changed := seeds["$mod"]; changed {
		t.Fatal("classifier mutated injected seeds")
	}
	if strings.Contains(include.includes[0], "$HOME") {
		t.Fatal("include directive retained unresolved seed")
	}
}

func TestSwayClassifierBoundsExpandedInclude(t *testing.T) {
	parser := newSwayConfigClassifier(map[string]string{"$part": strings.Repeat("x", maxSwayConfigLine)}).source("/fixture/config")
	facts := parser.line(swayLogicalLine{text: "include $part/$part", start: 1})
	if len(facts.includes) != 0 || len(facts.limitations) == 0 {
		t.Fatalf("unbounded variable expansion became an include directive: includes=%d limitations=%d", len(facts.includes), len(facts.limitations))
	}
}

func TestSwayClassifierBoundsExpandedStartupPrefix(t *testing.T) {
	prefix, suffix := ` t'r'u'e' "`, `"; sway-session daemon`
	body := prefix + strings.Repeat("x", maxSwayConfigLine-len(prefix)-len(suffix)-1) + suffix
	evidence := classifySwaySource(t, healthySwayConfig()+"exec {\n"+body+"\n}\n", nil)
	if evidence.uncertaintyCounts[integrationDaemon] == 0 || evidence.uncertaintyCounts[integrationPersistent] != 0 {
		t.Fatal("overlong startup prefix lost relevant uncertainty or its scope")
	}
}

func TestSwayClassifierUnrelatedShellStartup(t *testing.T) {
	for name, startup := range map[string]string{
		"quoted argument":   "exec sh -c 'notify-send \"unrelated; sway-session daemon\"'\n",
		"literal script":    "exec sh /fixture/startup.sh sway-session daemon\n",
		"script variable":   "exec sh $HOME/startup.sh\n",
		"argument variable": "exec sh -c 'notify-send \"$HOME\"'\n",
	} {
		t.Run(name, func(t *testing.T) {
			evidence := classifySwaySource(t, healthySwayConfig()+startup, map[string]string{"$HOME": "/fixture/home"})
			if evidence.unsupported != nil {
				t.Fatalf("unrelated startup tainted integration: %v", evidence.unsupported)
			}
		})
	}
}

func TestSwayClassifierDeduplicatesLimitationsNotDeclarations(t *testing.T) {
	parser := newSwayConfigClassifier(map[string]string{"$mod": "Mod4"}).source("/fixture/config")
	evidence := newSwayConfigEvidence()
	facts := parser.line(swayLogicalLine{text: "bindcode Mod4+36 exec foot", start: 7})
	evidence.record(facts)
	evidence.record(facts)
	for _, kind := range []integrationKind{integrationPersistent, integrationEphemeral} {
		if evidence.uncertaintyCounts[kind] != 1 || len(evidence.uncertain[kind]) != 1 {
			t.Fatalf("repeated limitation was not deduplicated for %s: %+v", integrationLabel(kind), evidence)
		}
	}
	for _, line := range []int{8, 9} {
		evidence.record(parser.line(swayLogicalLine{text: "exec sway-session daemon", start: line}))
	}
	if evidence.occurrenceCounts[integrationDaemon] != 2 || evidence.matchingCounts[integrationDaemon] != 2 {
		t.Fatalf("distinct declarations were deduplicated: %+v", evidence)
	}
}

func TestSwayClassifierSharedWrapperBudgets(t *testing.T) {
	deep := "notify-send sway-session"
	for range maxSwayWrapperDepth + 1 {
		deep = "env sh -c " + strconv.Quote(deep)
	}
	for name, content := range map[string]string{
		"mixed depth":   "exec " + deep + "\n",
		"expanded work": "exec env " + strings.Repeat("FOO=$large ", 10) + "notify-send sway-session\n",
	} {
		t.Run(name, func(t *testing.T) {
			evidence := classifySwaySource(t, healthySwayConfig()+content, map[string]string{"$large": strings.Repeat("x", maxSwayExpansionBytes/2)})
			if evidence.uncertaintyCounts[integrationDaemon] == 0 || evidence.uncertaintyCounts[integrationPersistent] != 0 {
				t.Fatalf("exhausted shared wrapper budget lost startup locality: %+v", evidence)
			}
		})
	}
}

func TestSwayClassifierBoundedDiagnosticDeduplication(t *testing.T) {
	evidence := newSwayConfigEvidence()
	for line := range maxSwayUncertaintyKeys + 10 {
		evidence.markUncertain([]integrationKind{integrationDaemon}, configLocation{path: "/fixture/config", line: line}, "unresolved startup")
	}
	if len(evidence.uncertaintySeen) != maxSwayUncertaintyKeys || !evidence.uncertaintySaturated[integrationDaemon] || len(evidence.uncertain[integrationDaemon]) != maxSwayEvidenceItems {
		t.Fatalf("diagnostic deduplication was not bounded: keys=%d retained=%d", len(evidence.uncertaintySeen), len(evidence.uncertain[integrationDaemon]))
	}
	// A newly affected requirement must still become unknown after saturation.
	evidence.markUncertain([]integrationKind{integrationRestore}, configLocation{path: "/fixture/config", line: 99}, "unresolved restore")
	if evidence.uncertaintyCounts[integrationRestore] == 0 {
		t.Fatal("deduplication saturation hid a new unknown requirement")
	}
}

func TestSwayClassifierUnresolvedStartupForms(t *testing.T) {
	for name, extra := range map[string]string{
		"unquoted later command":   "exec true; sway-session daemon\n",
		"shell control form":       "exec sh -c 'if true; then \"sway-session\" daemon; fi'\n",
		"variable assignment name": "set $$mod Mod1\n",
	} {
		t.Run(name, func(t *testing.T) {
			evidence := classifySwaySource(t, healthySwayConfig()+extra, nil)
			if evidence.unsupported == nil {
				t.Fatalf("unresolved relevant declaration was ignored: %q", extra)
			}
		})
	}
}

func TestSwayClassifierLiteralShellRelevance(t *testing.T) {
	for _, test := range []struct {
		name, startup string
		relevant      bool
	}{
		{"unrelated redirection", "exec nm-applet >/dev/null 2>&1\n", false},
		{"unrelated conditional", "exec sh -c 'if true; then notify-send hello; fi'\n", false},
		{"unrelated branches", "exec sh -c 'if false; then notify-send hello; elif true; then nm-applet; else notify-send bye; fi'\n", false},
		{"unrelated redirect path", "exec sh -c 'notify-send hello >\"/tmp/sway-session daemon\" 2>&1'\n", false},
		{"unrelated malformed argument", "exec notify-send \"broken\n", false},
		{"unrelated malformed later argument", "exec true; notify-send \"broken\n", false},
		{"relevant redirection", "exec sh -c '\"sway-session\" daemon >/dev/null 2>&1'\n", true},
		{"relevant conditional", "exec sh -c 'if true; then \"sway-session\" daemon; fi'\n", true},
		{"relevant inactive branch", "exec sh -c 'if false; then sway-session daemon; else notify-send hello; fi'\n", true},
		{"relevant condition", "exec sh -c 'if sway-session daemon; then notify-send hello; fi'\n", true},
		{"dynamic condition", "exec sh -c 'if $unknown; then notify-send hello; fi'\n", true},
		{"dynamic redirect", "exec nm-applet >$(sway-session daemon)\n", true},
		{"variable argument command", "set $msg hello; sway-session daemon\nexec notify-send $msg\n", true},
		{"quoted variable substitution", "set $msg '$(sway-session daemon)'\nexec notify-send '$msg'\n", true},
		{"quoted variable backticks", "set $msg '`sway-session daemon`'\nexec notify-send '$msg'\n", true},
		{"incomplete conditional", "exec sh -c 'if true; then notify-send hello'\n", true},
		{"incomplete executable", "exec \"notify-send\n", true},
		{"incomplete shell envelope", "exec true; sh -c 'notify-send hello\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := classifySwaySource(t, "set $mod Mod4\n"+test.startup, nil)
			_, err := repairableMissing(swayConfigAnalysis{swayConfigEvidence: evidence})
			if (err != nil) != test.relevant {
				t.Fatalf("startup repair eligibility = %v, want relevance %v: %+v", err, test.relevant, evidence)
			}
			if evidence.uncertaintyCounts[integrationPersistent] != 0 {
				t.Fatal("shell startup uncertainty tainted shortcuts")
			}
		})
	}
}
