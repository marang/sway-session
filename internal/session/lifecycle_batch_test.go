package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

const lifecycleBatchSecondID ContextID = "00000002-e89b-42d3-a456-000000000002"
const lifecycleBatchThirdID ContextID = "00000003-e89b-42d3-a456-000000000003"

func TestArchiveObservedTerminalContextsAtMixedUTCAndNoOp(t *testing.T) {
	local := time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.FixedZone("CEST", 7200))
	earlier := local.UTC().Add(-time.Hour)
	active := testValidContext(testContextID)
	archived := testValidContext(lifecycleBatchSecondID)
	archived.State, archived.ArchivedAt = ContextArchived, &earlier
	archived.Lifecycle = &LifecycleTransition{Reason: LifecycleReasonExplicitArchive, At: earlier}
	unrelated := testValidContext(lifecycleBatchThirdID)
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{active, archived, unrelated}}
	original := registry.Contexts
	if err := ArchiveObservedTerminalContextsAt(&registry, []ContextID{active.ID, archived.ID}, local); err != nil {
		t.Fatal(err)
	}
	got := registry.Contexts[0]
	if got.State != ContextArchived || got.ArchivedAt == nil || *got.ArchivedAt != local.UTC() || got.ArchivedAt.Location() != time.UTC || got.Lifecycle == nil || got.Lifecycle.Reason != LifecycleReasonObservedTerminalClose || got.Lifecycle.At != local.UTC() || got.Lifecycle.At.Location() != time.UTC {
		t.Fatalf("unexpected observed archive: %+v", got)
	}
	if !reflect.DeepEqual(registry.Contexts[1], archived) || !reflect.DeepEqual(registry.Contexts[2], unrelated) {
		t.Fatal("archived or unrelated metadata changed")
	}
	if !reflect.DeepEqual(original, []Context{active, archived, unrelated}) {
		t.Fatal("batch mutated the original context slice")
	}
	before := registry.Contexts
	if err := ArchiveObservedTerminalContextsAt(&registry, []ContextID{active.ID, archived.ID}, time.Time{}); err != nil {
		t.Fatalf("archive no-op requires fabricated time: %v", err)
	}
	if !reflect.DeepEqual(registry.Contexts, before) {
		t.Fatal("archive no-op rewrote timestamps or reasons")
	}
}

func TestArchiveObservedTerminalContextsAtLegacyNoOp(t *testing.T) {
	c := testValidContext(testContextID)
	c.State = ContextArchived
	registry := Registry{Version: 5, Contexts: []Context{c}}
	if err := ArchiveObservedTerminalContextsAt(&registry, []ContextID{c.ID}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registry.Contexts[0], c) || registry.Contexts[0].Lifecycle != nil || registry.Contexts[0].ArchivedAt != nil {
		t.Fatal("legacy no-op fabricated metadata")
	}
}

func TestArchiveObservedTerminalContextsAtAtomicRollback(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		ids         []ContextID
		at          time.Time
		mutate      func(*Registry)
		wantMissing bool
	}{
		{name: "unrelated invalid launcher", ids: []ContextID{testContextID, lifecycleBatchSecondID}, at: now, mutate: func(r *Registry) { r.Contexts[2].Launcher.Cwd = "relative" }},
		{name: "unrelated invalid lifecycle", ids: []ContextID{testContextID}, at: now, mutate: func(r *Registry) {
			r.Contexts[2].Lifecycle = &LifecycleTransition{Reason: LifecycleReasonExplicitArchive, At: now}
		}},
		{name: "unknown ID", ids: []ContextID{testContextID, ContextID("00000004-e89b-42d3-a456-000000000004")}, at: now, wantMissing: true},
		{name: "label is not ID", ids: []ContextID{testContextID, "duplicate-label"}, at: now},
		{name: "duplicate requested ID", ids: []ContextID{testContextID, testContextID}, at: now},
		{name: "ambiguous registry ID", ids: []ContextID{testContextID}, at: now, mutate: func(r *Registry) { r.Contexts[2].ID = testContextID }},
		{name: "desktop rejected", ids: []ContextID{testContextID, lifecycleBatchSecondID}, at: now, mutate: func(r *Registry) {
			desktop := desktopApplicationContext("example.desktop", "example.app")
			desktop.ID = lifecycleBatchSecondID
			r.Contexts[1] = desktop
		}},
		{name: "archived desktop rejected", ids: []ContextID{testContextID, lifecycleBatchSecondID}, at: now, mutate: func(r *Registry) {
			desktop := desktopApplicationContext("example.desktop", "example.app")
			desktop.ID, desktop.State = lifecycleBatchSecondID, ContextArchived
			r.Contexts[1] = desktop
		}},
		{name: "missing terminal", ids: []ContextID{testContextID, lifecycleBatchSecondID}, at: now, mutate: func(r *Registry) { r.Contexts[1].Launcher.Terminal = nil }},
		{name: "zero transition time", ids: []ContextID{testContextID}},
		{name: "invalid version", ids: []ContextID{testContextID}, at: now, mutate: func(r *Registry) { r.Version = 99 }},
		{name: "unknown state", ids: []ContextID{testContextID, lifecycleBatchSecondID}, at: now, mutate: func(r *Registry) { r.Contexts[1].State = "unknown" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := Registry{Version: 5, Contexts: []Context{testValidContext(testContextID), testValidContext(lifecycleBatchSecondID), testValidContext(lifecycleBatchThirdID)}}
			registry.Contexts[0].Label, registry.Contexts[1].Label = "duplicate-label", "duplicate-label"
			if tt.mutate != nil {
				tt.mutate(&registry)
			}
			before, err := json.Marshal(registry)
			if err != nil {
				t.Fatal(err)
			}
			originalSlice := registry.Contexts
			err = ArchiveObservedTerminalContextsAt(&registry, tt.ids, tt.at)
			if err == nil {
				t.Fatal("invalid batch accepted")
			}
			if tt.wantMissing && !errors.Is(err, ErrContextNotFound) {
				t.Fatalf("missing ID returned %v", err)
			}
			after, marshalErr := json.Marshal(registry)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if string(before) != string(after) || &registry.Contexts[0] != &originalSlice[0] {
				t.Fatalf("failed batch mutated registry: before=%s after=%s", before, after)
			}
		})
	}
	if err := ArchiveObservedTerminalContextsAt(nil, nil, now); err == nil {
		t.Fatal("nil registry accepted")
	}
}

func TestArchiveObservedTerminalContextsAtBoundedBatchWithoutInventoryCap(t *testing.T) {
	registry := Registry{Version: 5, Contexts: make([]Context, 513)}
	ids := make([]ContextID, 64)
	for i := range registry.Contexts {
		id := ContextID(fmt.Sprintf("%08x-e89b-42d3-a456-%012x", i+1, i+1))
		registry.Contexts[i] = testValidContext(id)
		if i < len(ids) {
			ids[i] = id
		}
	}
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	if err := ArchiveObservedTerminalContextsAt(&registry, ids, now); err != nil {
		t.Fatal(err)
	}
	for i, c := range registry.Contexts {
		if i < len(ids) && c.State != ContextArchived {
			t.Fatalf("requested context %d not archived", i)
		}
		if i >= len(ids) && (c.State != ContextActive || c.Lifecycle != nil || c.ArchivedAt != nil) {
			t.Fatalf("unrelated context %d changed", i)
		}
	}
}
