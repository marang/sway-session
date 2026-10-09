package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorUnrelatedColorAndCommentLinesPreserveOwnedSourceEvidence(t *testing.T) {
	for _, source := range []string{
		"set $color #222222\n",
		"set $color \"#1DD765FF\"\n",
		"# include missing.conf\n \t# set $broken \"unterminated\n",
		"bar {\n colors {\n background #202020\n }\n}\n",
	} {
		root := writeStandardSwayConfig(t, ShortcutsNone, true)
		source += "include " + doctorSnippetName + "\n"
		if err := os.WriteFile(root, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]
		if check.Status != OK {
			t.Fatalf("unrelated color/comment tainted standard source: %+v", check)
		}
		if after, err := os.ReadFile(root); err != nil || string(after) != source {
			t.Fatalf("inspection modified main: %v", err)
		}
	}
}

func TestDoctorStandardSiblingInDirectoryWithHash(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "parts#colors")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(directory, "config")
	snippet := filepath.Join(directory, doctorSnippetName)
	if err := os.WriteFile(root, []byte("include \""+snippet+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snippet, renderManagedSnippet("/usr/bin/sway-session", integrationOrder), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := inspectSwayConfig(t.Context(), Options{SwayConfigPath: root})[0]; check.Status != OK {
		t.Fatalf("literal hash path lost source evidence: %+v", check)
	}
}
