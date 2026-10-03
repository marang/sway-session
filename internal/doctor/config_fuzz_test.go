package doctor

import (
	"reflect"
	"strings"
	"testing"
)

func FuzzSwayConfigClassifier(f *testing.F) {
	for _, seed := range []string{
		healthySwayConfig(),
		"exec sh -c '\"sway-session\" daemon'\n",
		"exec sh -c 'notify-send \"unrelated; sway-session daemon\"'\n",
		"exec true; sway-session daemon\n",
		"exec env -S 'FOO=bar sh -c \"sway-session daemon\"'\n",
		"exec sh -c 'if true; then sway-session daemon; fi'\n",
		"exec sh -c 'if true; then notify-send hello; fi'\n",
		"exec nm-applet >/dev/null 2>&1\n",
		"exec sh -c '\"sway-session\" daemon >/dev/null 2>&1'\n",
		"exec true; sway-session daemon; notify-send \"broken\n",
		"set $msg hello; sway-session daemon\nexec notify-send $msg\n",
		"input * {\n xkb_layout \"broken\n}\n" + healthySwayConfig(),
		"bar {\n status_command printf '{'\n}\n",
		"bindsym --inhibited {\n $mod+Return exec \\\n sway-session terminal --new\n}\n",
		"mode \"default\" {\n bindsym $mod+Return exec foot }\n",
		"set $part $large\ninclude $large/$large\n",
		"output *\n# comment between header and brace\n{\n mode 1920x1080\n}\n",
	} {
		f.Add(seed)
	}
	prefix, suffix := ` t'r'u'e' "`, `"; sway-session daemon`
	f.Add("exec {\n" + prefix + strings.Repeat("x", maxSwayConfigLine-len(prefix)-len(suffix)-1) + suffix + "\n}\n")
	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > 2*maxSwayConfigLine {
			t.Skip()
		}
		run := func() swayConfigEvidence {
			classifier := newSwayConfigClassifier(map[string]string{"$mod": "Mod4", "$HOME": "/fixture/home", "$large": strings.Repeat("x", maxSwayExpansionBytes/2)})
			parser := classifier.source("/fixture/config")
			evidence := newSwayConfigEvidence()
			err := forEachSwayLogicalLine([]byte(content), func(line swayLogicalLine) bool {
				facts := parser.line(line)
				bytes := 0
				for _, include := range facts.includes {
					bytes += len(include) + 1
				}
				if bytes > maxSwayExpansionBytes || len(parser.blocks) > maxSwayBlockDepth {
					t.Fatal("classifier exceeded declaration or scope bounds")
				}
				evidence.record(facts)
				return true
			})
			if err != nil {
				evidence.markUncertainAll(configLocation{path: "/fixture/config"}, err.Error())
			}
			evidence.record(parser.finish())
			for _, kind := range integrationOrder {
				if len(evidence.occurrences[kind]) > maxSwayEvidenceItems || len(evidence.uncertain[kind]) > maxSwayEvidenceItems {
					t.Fatal("classifier exceeded retained evidence bounds")
				}
				if evidence.uncertaintyCounts[kind] != 0 {
					if _, err := repairableMissing(swayConfigAnalysis{swayConfigEvidence: evidence}); err == nil {
						t.Fatal("unknown requirement allowed automatic repair")
					}
				}
			}
			return evidence
		}
		if !reflect.DeepEqual(run(), run()) {
			t.Fatal("classification was not deterministic")
		}
	})
}

func FuzzSwayConfigLexer(f *testing.F) {
	for _, seed := range []string{"", "exec sway-session daemon", "printf '{'", "printf \\}", "exec sh -c '\"sway-session\" daemon'", "input * {", "}\ninclude hidden.conf", "bindsym $mod+Return exec \\", "exec \"broken", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 2*maxSwayConfigLine {
			t.Skip()
		}
		lexed := lexSwayLine(line)
		if !reflect.DeepEqual(lexed, lexSwayLine(line)) || len(lexed.tokens) != len(lexed.starts) {
			t.Fatal("lexer returned inconsistent token locations")
		}
		previous := -1
		for _, start := range lexed.starts {
			if start <= previous || start >= len(line) {
				t.Fatal("lexer lost original token positions")
			}
			previous = start
		}
	})
}
