package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorColorValuesPreserveIntegrationFacts(t *testing.T) {
	for name, value := range map[string]string{
		"RGB":     "#222222",
		"RGBA":    "#1DD765FF",
		"quoted":  `"#222222"`,
		"escaped": `\#222222`,
	} {
		t.Run(name, func(t *testing.T) {
			content := "set $color " + value + "\n" + healthySwayConfig()
			path := writeSwayConfig(t, content)
			check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: path})[0]
			if check.Status != OK || check.FixID != "" {
				t.Fatalf("ordinary color value tainted integration facts: %+v", check)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != content {
				t.Fatalf("inspection changed the configuration: %v", err)
			}
		})
	}
}

func TestDoctorFullLineCommentsPreserveIntegrationFacts(t *testing.T) {
	content := "# exec sway-session daemon\n" +
		" \t# include missing.conf\n" +
		" \t# set $broken \"unterminated\n" + healthySwayConfig()
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: writeSwayConfig(t, content)})[0]
	if check.Status != OK || check.FixID != "" {
		t.Fatalf("full-line comments changed integration facts: %+v", check)
	}
}

func TestDoctorIncludePathWithHashPreservesIntegrationFacts(t *testing.T) {
	root := writeSwayConfig(t, "include parts#colors.conf\n")
	part := filepath.Join(filepath.Dir(root), "parts#colors.conf")
	if err := os.WriteFile(part, []byte("set $color #222222\n"+healthySwayConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
	if check.Status != OK || check.FixID != "" {
		t.Fatalf("literal hash in include path lost integration facts: %+v", check)
	}
}
