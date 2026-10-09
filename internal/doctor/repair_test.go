package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairPlanPreviewApplyBackupAndIdempotence(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	if err := os.Mkdir(filepath.Join(directory, "config.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := "set $mod Mod4\nset $private super-secret-value\n"
	if err := os.WriteFile(root, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "sway-session")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n: > \"$0.started\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(directory, "unrelated.conf")
	if err := os.WriteFile(unrelated, []byte("set $private untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root, Executable: executable})

	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || len(plan.edits) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, change := range plan.Changes {
		if strings.Contains(change.Preview, "super-secret") {
			t.Fatalf("preview leaked surrounding configuration: %+v", change)
		}
	}
	if got := plan.Changes[1].Preview; !strings.HasPrefix(got, "+ include ") || strings.Contains(got, "set $private") {
		t.Fatalf("root preview is not narrow: %q", got)
	}

	result, err := service.Apply(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != swayIntegrationFixID || len(result.Backups) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	backup, err := os.ReadFile(result.Backups[0])
	if err != nil || string(backup) != secret {
		t.Fatalf("backup mismatch: %q, %v", backup, err)
	}
	assertMode(t, result.Backups[0], 0o600)
	snippet := filepath.Join(directory, "config.d", doctorSnippetName)
	assertMode(t, snippet, 0o600)
	rootAfter, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rootAfter), secret) || strings.Count(string(rootAfter), "include ") != 1 {
		t.Fatalf("root content not narrowly appended: %q", rootAfter)
	}
	if _, err := os.Stat(executable + ".started"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair executed the pinned command: %v", err)
	}
	unrelatedAfter, err := os.ReadFile(unrelated)
	if err != nil || string(unrelatedAfter) != "set $private untouched\n" {
		t.Fatalf("repair changed unrelated configuration: %q, %v", unrelatedAfter, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 5 {
		t.Fatalf("repair wrote beyond planned files and backup: %v, %v", entries, err)
	}
	managed, err := os.ReadFile(snippet)
	if err != nil || !strings.Contains(string(managed), "exec --no-startup-id "+executable+" daemon\n") ||
		!strings.Contains(string(managed), "exec --no-startup-id "+executable+" restore\n") || strings.Contains(string(managed), "exec_always") {
		t.Fatalf("repair changed pinned startup commands: %q, %v", managed, err)
	}
	check := inspectSwayConfig(context.Background(), service.options)[0]
	if check.Status != OK {
		t.Fatalf("repair did not converge: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil || !strings.Contains(err.Error(), "not needed") {
		t.Fatalf("idempotent plan unexpectedly available: %v", err)
	}
}

func TestRepairAdoptionDoesNotClassifyOrRewriteExistingIntegration(t *testing.T) {
	original := "set $mod Mod4\n" + healthySwayConfig() + "exec sh -c 'sway-session daemon'\ninclude *.conf\n"
	root := writeSwayConfig(t, original)
	service := New(Options{SwayConfigPath: root, Executable: "/usr/bin/sway-session"})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 || plan.Changes[0].Preview != string(renderManagedSnippet("/usr/bin/sway-session", []integrationKind{integrationDaemon, integrationRestore})) {
		t.Fatalf("adoption must create the complete startup-only standard profile: %+v", plan)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(root)
	if err != nil || !strings.HasPrefix(string(after), original) {
		t.Fatalf("adoption rewrote existing declarations: %q, %v", after, err)
	}
}

func TestRepairApplyExplainsSourceAndActivationLimits(t *testing.T) {
	root := writeSwayConfig(t, "# root\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"Reload Sway manually", "removing owned bindings does not establish", "Startup commands run in the next Sway session", "reload does not run them", "Review previous integration and effective load order manually"} {
		if !strings.Contains(result.Message, fragment) {
			t.Errorf("repair guidance omitted %q: %q", fragment, result.Message)
		}
	}
}

func TestRepairShortcutSelectionPreservesOrChangesCompleteProfiles(t *testing.T) {
	for _, existing := range []ShortcutSelection{ShortcutsNone, ShortcutsDefault} {
		for _, requested := range []ShortcutSelection{ShortcutsUnspecified, ShortcutsNone, ShortcutsDefault} {
			t.Run(string(existing)+" to "+string(requested), func(t *testing.T) {
				root := writeStandardSwayConfig(t, existing, true)
				snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
				service := New(Options{SwayConfigPath: root, Executable: "/different/sway-session"})
				plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: requested})
				if requested == ShortcutsUnspecified || requested == existing {
					if err == nil || !strings.Contains(err.Error(), "not needed") {
						t.Fatalf("unchanged profile offered rewrite: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(plan.Changes) != 1 || plan.Changes[0].Path != snippet {
					t.Fatalf("profile change escaped owned snippet: %+v", plan)
				}
				originalRoot, _ := os.ReadFile(root)
				result, err := service.Apply(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Backups) != 1 {
					t.Fatalf("expected managed-source backup: %+v", result)
				}
				parsed, err := parseManagedSnippet(plan.edits[0].newContent)
				if err != nil || parsed.shortcuts != requested || parsed.executable != "/usr/bin/sway-session" {
					t.Fatalf("profile change changed pinned executable or selection: %+v, %v", parsed, err)
				}
				if after, err := os.ReadFile(root); err != nil || string(after) != string(originalRoot) {
					t.Fatalf("profile change rewrote root: %q, %v", after, err)
				}
			})
		}
	}
}

func TestRepairAdoptionPreservesExistingProfileWhenIncludeMissing(t *testing.T) {
	for _, existing := range []ShortcutSelection{ShortcutsNone, ShortcutsDefault} {
		root := writeStandardSwayConfig(t, existing, false)
		service := New(Options{SwayConfigPath: root})
		if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{}); err == nil || !strings.Contains(err.Error(), "explicit adoption") {
			t.Fatalf("missing include accepted without adoption: %v", err)
		}
		plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 1 || plan.Changes[0].Path != root {
			t.Fatalf("unspecified shortcuts changed existing snippet: %+v", plan)
		}
		if _, err := service.Apply(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		check := inspectSwayConfig(t.Context(), service.options)[0]
		if check.Status != OK || !containsEvidence(check.Evidence, standardProfileEvidence(existing)) {
			t.Fatalf("adoption changed existing profile: %+v", check)
		}
	}
}

func TestRepairRequiresExplicitAdoptionForCreationAndRecovery(t *testing.T) {
	for _, main := range []string{"# root\n", "include config.d/" + doctorSnippetName + "\n"} {
		root := writeSwayConfig(t, main)
		service := New(Options{SwayConfigPath: root})
		for _, selection := range []ShortcutSelection{ShortcutsUnspecified, ShortcutsNone, ShortcutsDefault} {
			if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{Shortcuts: selection}); err == nil || !strings.Contains(err.Error(), "explicit adoption") {
				t.Fatalf("shortcut choice granted adoption: %v", err)
			}
		}
		if after, err := os.ReadFile(root); err != nil || string(after) != main {
			t.Fatalf("rejected adoption changed root: %q, %v", after, err)
		}
	}
}

func TestRepairAdoptionShortcutSelection(t *testing.T) {
	for _, selection := range []ShortcutSelection{ShortcutsUnspecified, ShortcutsNone, ShortcutsDefault} {
		root := writeSwayConfig(t, "# no modifier declaration\n")
		service := New(Options{SwayConfigPath: root})
		plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: selection})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseManagedSnippet(plan.edits[0].newContent)
		want := selection
		if want == ShortcutsUnspecified {
			want = ShortcutsNone
		}
		if err != nil || parsed.shortcuts != want {
			t.Fatalf("adoption selected wrong profile: %+v, %v", parsed, err)
		}
	}
	root := writeSwayConfig(t, "# root\n")
	service := New(Options{SwayConfigPath: root})
	if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: "custom"}); err == nil {
		t.Fatal("unknown shortcut selection accepted")
	}
	if _, err := service.Plan(t.Context(), "unknown", RepairOptions{AdoptStandard: true}); err == nil {
		t.Fatal("unknown repair accepted")
	}
}

func TestRepairRecoversMissingIncludedManagedSnippet(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "config")
	if err := os.Mkdir(filepath.Join(directory, "config.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	snippet := filepath.Join(directory, "config.d", doctorSnippetName)
	includeLine, err := renderIncludeLine(snippet)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("set $mod Mod4\n"+includeLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Path != snippet {
		t.Fatalf("repair should only recover included snippet: %+v", plan.Changes)
	}
	if _, err := service.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestRepairRefusesUnknownManagedSnippetEdits(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	if err := os.WriteFile(snippet, []byte(doctorHeader+"set $manual yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{SwayConfigPath: root})
	check := inspectSwayConfig(context.Background(), service.options)[0]
	if check.FixID != "" || !strings.Contains(check.Hint, "manual") {
		t.Fatalf("inspection offered unsafe overwrite: %+v", check)
	}
	if _, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil || !strings.Contains(err.Error(), "manual edits") {
		t.Fatalf("manual edits were not refused: %v", err)
	}
}

func TestRepairRejectsStaleFileAndDirectoryPlans(t *testing.T) {
	t.Run("file changed", func(t *testing.T) {
		root := writeSwayConfig(t, "set $mod Mod4\n")
		service := New(Options{SwayConfigPath: root})
		plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root, []byte("set $mod Mod1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale file plan accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale plan wrote snippet: %v", err)
		}
	})

	t.Run("directory replaced", func(t *testing.T) {
		parent := t.TempDir()
		directory := filepath.Join(parent, "sway")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(directory, "config")
		if err := os.Mkdir(filepath.Join(directory, "config.d"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root, []byte("set $mod Mod4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		service := New(Options{SwayConfigPath: root})
		plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
		if err != nil {
			t.Fatal(err)
		}
		old := directory + "-old"
		if err := os.Rename(directory, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root, []byte("set $mod Mod4\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(context.Background(), plan); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("replaced directory accepted: %v", err)
		}
	})
}

func TestRepairRefusesUnsafeExecutableAndCancellation(t *testing.T) {
	root := writeSwayConfig(t, "set $mod Mod4\n")
	service := New(Options{SwayConfigPath: root, Executable: "/usr/bin/sway-session;touch-pwned"})
	if _, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
		t.Fatalf("unsafe executable accepted: %v", err)
	}

	service = New(Options{SwayConfigPath: root})
	plan, err := service.Plan(context.Background(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Apply(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled apply returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled apply wrote snippet: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode %04o, want %04o", path, got, want)
	}
}

func TestRepairPrivateRequestAndPreviewCannotGrantAuthority(t *testing.T) {
	root := writeSwayConfig(t, "# selected main\n")
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var untrusted Plan
	if err := json.Unmarshal(encoded, &untrusted); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(t.Context(), untrusted); err == nil || !strings.Contains(err.Error(), "trusted private") {
		t.Fatalf("public fields granted authority=%v", err)
	}
	tampered := plan
	tampered.Changes = append([]FileChange(nil), plan.Changes...)
	tampered.Changes[0].Preview = "pretend no shortcuts"
	if _, err := service.Apply(t.Context(), tampered); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed preview accepted=%v", err)
	}
	tampered = plan
	tampered.ID = "other"
	if _, err := service.Apply(t.Context(), tampered); err == nil {
		t.Fatal("changed exported ID accepted")
	}
	tampered = plan
	tampered.request = RepairOptions{}
	if _, err := service.Apply(t.Context(), tampered); err == nil {
		t.Fatal("adoption loss accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("untrusted apply wrote file")
	}
}

func TestRepairChangesProfileAndAppendsIncludeTogether(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsDefault, false)
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsNone})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("expected profile and include changes: %+v", plan)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Backups) != 2 {
		t.Fatalf("both existing files need private backups: %+v", result)
	}
	content, err := os.ReadFile(snippet)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseManagedSnippet(content)
	if err != nil || parsed.shortcuts != ShortcutsNone {
		t.Fatalf("profile change not applied: %+v, %v", parsed, err)
	}
	if check := inspectSwayConfig(t.Context(), service.options)[0]; check.Status != OK || check.AdoptionRequired {
		t.Fatalf("combined change did not converge: %+v", check)
	}
}

func TestRepairRefusesNonStandaloneIncludeAppend(t *testing.T) {
	for name, source := range map[string]string{
		"trailing continuation no newline":   "exec notify-send \\",
		"trailing continuation with newline": "exec notify-send \\\n",
		"indented comment continuation":      "  # note \\\n",
		"indented comment without newline":   "\t# note \\",
		"continued comment at EOF":           "exec /usr/bin/true \\\n# note \\\n",
		"NUL hidden mode opener":             "mode default {\x00opaque\n",
		"unfinished mode":                    "mode resize {\n bindsym Return mode default\n",
		"unfinished startup block":           "exec {\n notify-send hello\n",
		"oversized logical line":             strings.Repeat("x", maxSwayConfigLine+1) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeSwayConfig(t, source)
			service := New(Options{SwayConfigPath: root})
			check := inspectSwayConfig(t.Context(), service.options)[0]
			if check.Status != Unavailable || check.FixID != "" || check.AdoptionRequired || !strings.Contains(check.Hint, "unavailable: cannot append a direct include") {
				t.Fatalf("unsafe append lacked actionable source-only diagnosis: %+v", check)
			}
			if name != "oversized logical line" && !strings.Contains(check.Hint, "manually first") {
				t.Fatalf("unsafe EOF omitted concrete manual correction: %+v", check)
			}
			if _, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true}); err == nil {
				t.Fatal("unsafe EOF allowed a non-standalone include append")
			}
			if after, err := os.ReadFile(root); err != nil || string(after) != source {
				t.Fatalf("failed preview changed root: %q, %v", after, err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed preview created snippet: %v", err)
			}
		})
	}
}

func TestRepairAllowsAppendAfterBlankLineEndsContinuation(t *testing.T) {
	source := "exec notify-send \\\n\n"
	root := writeSwayConfig(t, source)
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true})
	if err != nil {
		t.Fatalf("blank physical line ends continuation and leaves a standalone append boundary: %v", err)
	}
	if _, err := service.Apply(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(root)
	if err != nil || !strings.HasPrefix(string(after), source) {
		t.Fatalf("source preceding include changed: %q, %v", after, err)
	}
	if line := literalDirectInclude(after, root, filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)); line != 3 {
		t.Fatalf("include was not standalone after blank physical line: %d", line)
	}
}

func TestRepairNormalizedLiteralIncludesDoNotAppendDuplicates(t *testing.T) {
	for _, include := range []string{"include ./config.d/" + doctorSnippetName + "\n", "include \"./config.d/" + doctorSnippetName + "\"\n", "include .//config.d/" + doctorSnippetName + "\n"} {
		root := writeStandardSwayConfig(t, ShortcutsNone, true)
		if err := os.WriteFile(root, []byte(include), 0o600); err != nil {
			t.Fatal(err)
		}
		service := New(Options{SwayConfigPath: root})
		check := inspectSwayConfig(t.Context(), service.options)[0]
		if check.Status != OK || check.AdoptionRequired {
			t.Fatalf("normalized literal include not recognized: %+v", check)
		}
		plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsDefault})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Changes) != 1 || plan.Changes[0].Path == root {
			t.Fatalf("normalized literal appended another direct include: %+v", plan)
		}
		if _, err := service.Apply(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		if after, err := os.ReadFile(root); err != nil || string(after) != include {
			t.Fatalf("normalized source include was rewritten: %q, %v", after, err)
		}
	}
}

func TestRepairPreservesExistingFileModesAndMakesPrivateBackups(t *testing.T) {
	root := writeStandardSwayConfig(t, ShortcutsDefault, false)
	snippet := filepath.Join(filepath.Dir(root), "config.d", doctorSnippetName)
	for _, path := range []string{root, snippet} {
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	service := New(Options{SwayConfigPath: root})
	plan, err := service.Plan(t.Context(), swayIntegrationFixID, RepairOptions{AdoptStandard: true, Shortcuts: ShortcutsNone})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, snippet} {
		assertMode(t, path, 0o640)
	}
	if len(result.Backups) != 2 {
		t.Fatalf("expected both existing-source backups: %+v", result)
	}
	for _, path := range result.Backups {
		assertMode(t, path, 0o600)
	}
}
