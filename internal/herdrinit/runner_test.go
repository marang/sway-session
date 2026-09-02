package herdrinit

import (
	"strings"
	"testing"
)

func TestExecRunnerEnvironmentRemovesCallerControlVariables(t *testing.T) {
	t.Setenv("SAFE_VALUE", "retained")
	t.Setenv("HERDR_CONFIG_PATH", "/tmp/untrusted-herdr-config")
	t.Setenv("HERDR_PANE_ID", "untrusted-pane")
	t.Setenv("CODEX_THREAD_ID", "untrusted-thread")
	t.Setenv("LD_PRELOAD", "/tmp/untrusted.so")
	t.Setenv("LD_LIBRARY_PATH", "/tmp/untrusted-libraries")

	environment, err := (ExecRunner{ConfigFile: "/trusted/herdr/config.toml"}).environment()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string][]string)
	for _, value := range environment {
		name, content, found := strings.Cut(value, "=")
		if !found {
			t.Fatalf("environment entry has no assignment: %q", value)
		}
		values[name] = append(values[name], content)
	}
	if got := values["SAFE_VALUE"]; len(got) != 1 || got[0] != "retained" {
		t.Fatalf("safe environment value not preserved: %v", got)
	}
	if got := values["HERDR_CONFIG_PATH"]; len(got) != 1 || got[0] != "/trusted/herdr/config.toml" {
		t.Fatalf("trusted Herdr config path not installed exactly once: %v", got)
	}
	for _, name := range []string{"HERDR_PANE_ID", "CODEX_THREAD_ID", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		if got := values[name]; len(got) != 0 {
			t.Fatalf("unsafe caller variable %s was retained: %v", name, got)
		}
	}
}

func TestExecRunnerEnvironmentRejectsUnsafeConfigPath(t *testing.T) {
	for _, path := range []string{"", "relative/config.toml", "/tmp/../config.toml", "/tmp/config.toml\nOTHER=value"} {
		if _, err := (ExecRunner{ConfigFile: path}).environment(); err == nil {
			t.Fatalf("unsafe config path %q was accepted", path)
		}
	}
}
