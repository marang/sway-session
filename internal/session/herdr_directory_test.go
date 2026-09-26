package session

import (
	"encoding/json"
	"testing"
)

func TestParseHerdrPaneDirectoryRequiresOneNonHomeDirectory(t *testing.T) {
	const home = "/home/example"
	tests := []struct {
		name      string
		result    string
		want      HerdrDirectoryObservation
		wantError bool
	}{
		{
			name:   "agent and shell share one project",
			result: `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"/home/example","foreground_cwd":"/work/project"},{"cwd":"/work/project","foreground_cwd":"/work/project"}]}}`,
			want:   HerdrDirectoryObservation{Directory: "/work/project"},
		},
		{
			name:   "foreground directory supersedes pane start directory",
			result: `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"/work/old","foreground_cwd":"/work/new"}]}}`,
			want:   HerdrDirectoryObservation{Directory: "/work/new"},
		},
		{
			name:   "only home",
			result: `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"/home/example"}]}}`,
		},
		{
			name:   "root is not a project",
			result: `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"/"}]}}`,
		},
		{
			name:   "different projects",
			result: `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"/work/first"},{"cwd":"/work/second"}]}}`,
			want:   HerdrDirectoryObservation{Ambiguous: true},
		},
		{
			name:      "invalid path",
			result:    `{"type":"session_snapshot","snapshot":{"panes":[{"cwd":"relative/project"}]}}`,
			wantError: true,
		},
		{
			name:      "missing panes",
			result:    `{"type":"session_snapshot","snapshot":{}}`,
			wantError: true,
		},
		{
			name:      "wrong result",
			result:    `{"type":"other","snapshot":{"panes":[]}}`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseHerdrPaneDirectory(json.RawMessage(test.result), home)
			if (err != nil) != test.wantError || got != test.want {
				t.Fatalf("parseHerdrPaneDirectory() = %+v, %v; want %+v, error=%t", got, err, test.want, test.wantError)
			}
		})
	}
}
