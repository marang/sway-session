package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Use literal wire versions so changes to the current version cannot silently
// turn the compatibility fixtures into current-version fixtures.
func migrationTestLayout(version int) LayoutSnapshot {
	return LayoutSnapshot{Version: version, Workspaces: []WorkspaceLayout{
		{
			Name: "98: tree", RestoreMode: WorkspaceRestoreLayout,
			Tiling: &LayoutNode{Layout: LayoutSplitHorizontal, Children: []LayoutNode{
				{ContextID: contextIDPointerValue(testContextID), Proportion: 0.3},
				{Layout: LayoutTabbed, Proportion: 0.7, Children: []LayoutNode{
					{ContextID: contextIDPointerValue(secondContextID), Proportion: 1, Fullscreen: FullscreenWorkspace},
				}},
			}},
			Floating: []LayoutNode{{
				ContextID: contextIDPointerValue(thirdContextID),
				Geometry:  &Geometry{X: -20, Y: 40, Width: 640, Height: 480},
			}},
			FocusedContext: contextIDPointerValue(secondContextID),
		},
		{
			Name: "99: placement", RestoreMode: WorkspaceRestorePlacementOnly,
			PlacementContexts: []ContextID{ContextID("44444444-4444-4444-8444-444444444444")},
		},
	}}
}

func migrationTestPayload(t *testing.T, value LayoutSnapshot) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func migrationTestScratchpadLayout() LayoutSnapshot {
	value := migrationTestLayout(2)
	value.Scratchpad = []ScratchpadPlacement{
		{ContextID: ContextID("55555555-5555-4555-8555-555555555555")},
		{ContextID: ContextID("66666666-6666-4666-8666-666666666666"), Visible: true, Workspace: "98: tree"},
	}
	return value
}

func TestLayoutMigrationV1PreservesExactTreeAndInput(t *testing.T) {
	old := migrationTestLayout(1)
	payload := migrationTestPayload(t, old)
	before := bytes.Clone(payload)
	got, err := decodeStoredLayoutSnapshot("test layout", 1, payload)
	if err != nil {
		t.Fatal(err)
	}
	want := old
	want.Version = 2
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migration changed the saved layout:\ngot  %+v\nwant %+v", got, want)
	}
	if !bytes.Equal(payload, before) || old.Version != 1 {
		t.Fatal("migration changed its input")
	}
	if err := old.Validate(); err == nil {
		t.Fatal("normal validation accepted v1")
	}
}

func TestLayoutMigrationEmptyV1(t *testing.T) {
	got, err := decodeStoredLayoutSnapshot("empty layout", 1, []byte(`{"version":1,"workspaces":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Workspaces == nil || len(got.Workspaces) != 0 || got.Scratchpad != nil {
		t.Fatalf("incorrect empty migration: %+v", got)
	}
}

func TestLayoutMigrationV2PreservesScratchpad(t *testing.T) {
	want := migrationTestScratchpadLayout()
	got, err := decodeStoredLayoutSnapshot("test layout", 2, migrationTestPayload(t, want))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("v2 decode = %+v, err=%v", got, err)
	}
}

type migrationInvalidCase struct {
	name            string
	encodingVersion int
	payload         string
	errorText       string
	unsupported     bool
}

func migrationInvalidCases() []migrationInvalidCase {
	return []migrationInvalidCase{
		{"v1 encoding v2 payload", 1, `{"version":2,"workspaces":[]}`, "does not match payload version", false},
		{"v2 encoding v1 payload", 2, `{"version":1,"workspaces":[]}`, "does not match payload version", false},
		{"unknown encoding", 99, `{"version":2,"workspaces":[]}`, "version", true},
		{"unknown payload", 2, `{"version":99,"workspaces":[]}`, "version", true},
		{"unknown matching versions", 99, `{"version":99,"workspaces":[]}`, "version", true},
		{"zero encoding", 0, `{"version":1,"workspaces":[]}`, "version", true},
		{"negative payload version", 1, `{"version":-1,"workspaces":[]}`, "version", true},
		{"missing payload version", 1, `{"workspaces":[]}`, "version", true},
		{"v1 populated scratchpad", 1, `{"version":1,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555"}]}`, "must not contain scratchpad", false},
		{"v1 empty scratchpad", 1, `{"version":1,"workspaces":[],"scratchpad":[]}`, "must not contain scratchpad", false},
		{"v1 null scratchpad", 1, `{"version":1,"workspaces":[],"scratchpad":null}`, "must not contain scratchpad", false},
		{"v1 malformed scratchpad", 1, `{"version":1,"workspaces":[],"scratchpad":{}}`, "must not contain scratchpad", false},
		{"v1 visible scratchpad", 1, `{"version":1,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555","visible":true,"workspace":"98"}]}`, "must not contain scratchpad", false},
		{"v1 missing workspaces", 1, `{"version":1}`, "workspaces array", false},
		{"v1 null workspaces", 1, `{"version":1,"workspaces":null}`, "workspaces array", false},
		{"v1 unknown field", 1, `{"version":1,"workspaces":[],"future":true}`, "unknown field", false},
		{"v2 unknown field", 2, `{"version":2,"workspaces":[],"future":true}`, "unknown field", false},
		{"v1 nested unknown field", 1, `{"version":1,"workspaces":[{"name":"98","future":true}]}`, "unknown field", false},
		{"v1 invalid workspace", 1, `{"version":1,"workspaces":[{"name":"98","restore_mode":"layout"}]}`, "managed window", false},
		{"v2 scratchpad unknown field", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555","future":true}]}`, "unknown field", false},
		{"v2 scratchpad invalid ID", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"invalid"}]}`, "canonical UUID", false},
		{"v2 duplicate scratchpad", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555"},{"context_id":"55555555-5555-4555-8555-555555555555"}]}`, "also appears", false},
		{"v2 scratchpad workspace overlap", 2, `{"version":2,"workspaces":[{"name":"98","restore_mode":"placement_only","placement_contexts":["55555555-5555-4555-8555-555555555555"]}],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555"}]}`, "also appears", false},
		{"v2 malformed scratchpad", 2, `{"version":2,"workspaces":[],"scratchpad":{}}`, "scratchpad", false},
		{"v2 visible scratchpad without workspace", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555","visible":true}]}`, "normal workspace", false},
		{"v2 hidden scratchpad with workspace", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555","workspace":"98"}]}`, "normal workspace", false},
		{"v2 visible scratchpad reserved workspace", 2, `{"version":2,"workspaces":[],"scratchpad":[{"context_id":"55555555-5555-4555-8555-555555555555","visible":true,"workspace":"__i3_scratch"}]}`, "normal workspace", false},
		{"trailing JSON", 1, `{"version":1,"workspaces":[]} {}`, "trailing JSON", false},
		{"malformed JSON", 1, `{"version":1,`, "decode", false},
	}
}

func assertMigrationError(t *testing.T, test migrationInvalidCase, err error, context string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), test.errorText) || !strings.Contains(err.Error(), context) {
		t.Fatalf("expected contextual %q error, got %v", test.errorText, err)
	}
	if test.unsupported {
		var versionError *UnsupportedVersionError
		if !errors.As(err, &versionError) {
			t.Fatalf("unsupported version error lost its type: %v", err)
		}
	}
}

func TestLayoutMigrationRejectsInvalidDocuments(t *testing.T) {
	for _, test := range migrationInvalidCases() {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeStoredLayoutSnapshot("test layout", test.encodingVersion, []byte(test.payload))
			assertMigrationError(t, test, err, "test layout")
			if !reflect.DeepEqual(got, LayoutSnapshot{}) {
				t.Fatalf("failed migration returned a partial snapshot: %+v", got)
			}
		})
	}
}

func TestLayoutMigrationRetainsPayloadBudget(t *testing.T) {
	payload := bytes.Repeat([]byte(" "), maxDatabasePayloadBytes+1)
	if _, err := decodeStoredLayoutSnapshot("oversized layout", 1, payload); err == nil || !strings.Contains(err.Error(), "payload is too large") {
		t.Fatalf("oversized layout accepted: %v", err)
	}
}
