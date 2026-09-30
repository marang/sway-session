package session

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTransitionArchiveActivateAndNoOp(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{testValidContext(testContextID)}}
	local := time.Date(2026, 9, 30, 12, 0, 0, 123, time.FixedZone("CEST", 7200))
	archived, err := SetContextStateAt(&registry, string(testContextID), ContextArchived, local)
	if err != nil {
		t.Fatal(err)
	}
	want := local.UTC()
	if archived.Lifecycle == nil || archived.Lifecycle.Reason != LifecycleReasonExplicitArchive || archived.Lifecycle.At != want || archived.Lifecycle.At.Location() != time.UTC || archived.ArchivedAt == nil || *archived.ArchivedAt != want {
		t.Fatalf("unexpected archive: %+v", archived)
	}
	repeated, err := SetContextStateWithReasonAt(&registry, string(testContextID), ContextArchived, LifecycleReasonObservedTerminalClose, time.Time{})
	if err != nil || !reflect.DeepEqual(repeated, archived) {
		t.Fatalf("archive no-op changed metadata: %+v, %v", repeated, err)
	}
	active, err := SetContextStateAt(&registry, string(testContextID), ContextActive, local.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if active.ArchivedAt != nil || active.Lifecycle == nil || active.Lifecycle.Reason != LifecycleReasonExplicitActivate || active.Lifecycle.At != want.Add(time.Hour) {
		t.Fatalf("unexpected activation: %+v", active)
	}
	repeated, err = SetContextStateAt(&registry, string(testContextID), ContextActive, time.Time{})
	if err != nil || !reflect.DeepEqual(repeated, active) {
		t.Fatalf("active no-op changed metadata: %+v, %v", repeated, err)
	}
}

func TestLifecycleObservedTerminalClose(t *testing.T) {
	registry := Registry{Version: ContextsSchemaVersion, Contexts: []Context{testValidContext(testContextID)}}
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	context, err := SetContextStateWithReasonAt(&registry, string(testContextID), ContextArchived, LifecycleReasonObservedTerminalClose, now)
	if err != nil {
		t.Fatal(err)
	}
	if context.Lifecycle == nil || context.Lifecycle.Reason != LifecycleReasonObservedTerminalClose || context.Lifecycle.At != now || context.ArchivedAt == nil || *context.ArchivedAt != now {
		t.Fatalf("unexpected observed close: %+v", context)
	}
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleTransitionInvalidMutationIsAtomic(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		state  ContextState
		reason LifecycleReason
		at     time.Time
		setup  func(*Registry)
	}{
		{name: "unknown reason", state: ContextArchived, reason: "arbitrary", at: now},
		{name: "empty reason", state: ContextArchived, at: now},
		{name: "archive with activation reason", state: ContextArchived, reason: LifecycleReasonExplicitActivate, at: now},
		{name: "activation with close reason", state: ContextActive, reason: LifecycleReasonObservedTerminalClose, at: now},
		{name: "activation with archive reason", state: ContextActive, reason: LifecycleReasonExplicitArchive, at: now},
		{name: "zero archive time", state: ContextArchived, reason: LifecycleReasonExplicitArchive},
		{name: "zero activation time", state: ContextActive, reason: LifecycleReasonExplicitActivate, setup: func(r *Registry) {
			r.Contexts[0].State = ContextArchived
			r.Contexts[0].ArchivedAt = &now
			r.Contexts[0].Lifecycle = &LifecycleTransition{Reason: LifecycleReasonExplicitArchive, At: now}
		}},
		{name: "invalid registry", state: ContextArchived, reason: LifecycleReasonExplicitArchive, at: now, setup: func(r *Registry) { r.Version = 999 }},
		{name: "invalid activation registry", state: ContextActive, reason: LifecycleReasonExplicitActivate, at: now.Add(time.Hour), setup: func(r *Registry) {
			r.Version = 999
			r.Contexts[0].State = ContextArchived
			r.Contexts[0].ArchivedAt = &now
			r.Contexts[0].Lifecycle = &LifecycleTransition{Reason: LifecycleReasonObservedTerminalClose, At: now}
		}},
		{name: "application close", state: ContextArchived, reason: LifecycleReasonObservedTerminalClose, at: now, setup: func(r *Registry) { r.Contexts[0] = desktopApplicationContext("example.desktop", "example.app") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Registry{Version: ContextsSchemaVersion, Contexts: []Context{testValidContext(testContextID)}}
			if tt.setup != nil {
				tt.setup(&r)
			}
			before, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SetContextStateWithReasonAt(&r, string(r.Contexts[0].ID), tt.state, tt.reason, tt.at); err == nil {
				t.Fatal("invalid transition accepted")
			}
			after, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatalf("failed transition changed registry:\nbefore %s\nafter %s", before, after)
			}
		})
	}
}

func TestLifecycleMetadataValidation(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Context)
	}{
		{"empty reason", func(c *Context) { c.Lifecycle.Reason = "" }},
		{"unknown reason", func(c *Context) { c.Lifecycle.Reason = "unbounded" }},
		{"zero timestamp", func(c *Context) { c.Lifecycle.At = time.Time{} }},
		{"non UTC", func(c *Context) { c.Lifecycle.At = now.In(time.FixedZone("offset", 3600)) }},
		{"noncanonical UTC", func(c *Context) { c.Lifecycle.At = now.In(time.FixedZone("UTC", 0)) }},
		{"reason state mismatch", func(c *Context) { c.Lifecycle.Reason = LifecycleReasonExplicitActivate }},
		{"missing archive time", func(c *Context) { c.ArchivedAt = nil }},
		{"different archive time", func(c *Context) { later := now.Add(time.Second); c.ArchivedAt = &later }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testValidContext(testContextID)
			c.State = ContextArchived
			c.ArchivedAt = &now
			c.Lifecycle = &LifecycleTransition{Reason: LifecycleReasonExplicitArchive, At: now}
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid lifecycle metadata accepted")
			}
		})
	}
}

func TestLifecycleLegacyRoundTripRemainsUnknown(t *testing.T) {
	for _, state := range []ContextState{ContextActive, ContextArchived} {
		t.Run(string(state), func(t *testing.T) {
			original := Registry{Version: 5, Contexts: []Context{testValidContext(testContextID)}}
			original.Contexts[0].State = state
			encoded, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "lifecycle") {
				t.Fatalf("legacy JSON fabricated metadata: %s", encoded)
			}
			var decoded Registry
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := decoded.Validate(); err != nil {
				t.Fatal(err)
			}
			if decoded.Version != 5 || decoded.Contexts[0].Lifecycle != nil {
				t.Fatalf("changed legacy state: %+v", decoded)
			}
			if _, err := SetContextStateAt(&decoded, string(testContextID), state, time.Now()); err != nil {
				t.Fatal(err)
			}
			roundTrip, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if string(roundTrip) != string(encoded) {
				t.Fatalf("legacy no-op changed durable JSON: %s", roundTrip)
			}
		})
	}
}

func TestLifecycleJSONRoundTrip(t *testing.T) {
	r := Registry{Version: 5, Contexts: []Context{testValidContext(testContextID)}}
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	if _, err := SetContextStateAt(&r, string(testContextID), ContextArchived, now); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"lifecycle":{"reason":"explicit_archive","at":"2026-09-30T10:00:00Z"}`) {
		t.Fatalf("unexpected lifecycle wire shape: %s", encoded)
	}
	var decoded Registry
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, decoded) {
		t.Fatalf("lifecycle did not round trip: %+v", decoded)
	}
}

func TestLifecycleLegacyZeroTimeActivationDoesNotInventHistory(t *testing.T) {
	r := Registry{Version: 5, Contexts: []Context{testValidContext(testContextID)}}
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	if _, err := SetContextStateAt(&r, string(testContextID), ContextArchived, now); err != nil {
		t.Fatal(err)
	}
	c, err := SetContextStateAt(&r, string(testContextID), ContextActive, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != ContextActive || c.ArchivedAt != nil || c.Lifecycle != nil {
		t.Fatalf("zero-time activation fabricated history: %+v", c)
	}
}

func TestLifecycleSQLiteRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	legacy := Registry{Version: 5, Contexts: []Context{testValidContext(testContextID)}}
	if err := RegistryStoreFor(root).Save(legacy); err != nil {
		t.Fatalf("save legacy registry: %v", err)
	}
	assertSnapshot := func(want Registry) {
		t.Helper()
		got, err := ReadRegistrySnapshot(root)
		if err != nil {
			t.Fatalf("reload registry: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Fatalf("stored registry differs:\ngot  %s\nwant %s", gotJSON, wantJSON)
		}
	}
	update := func(state ContextState, reason LifecycleReason, at time.Time) error {
		_, err := UpdateRegistryContext(t.Context(), root, func(registry *Registry) error {
			_, err := SetContextStateWithReasonAt(registry, string(testContextID), state, reason, at)
			return err
		})
		return err
	}
	assertSnapshot(legacy)

	local := time.Date(2026, 9, 30, 12, 0, 0, 123456789, time.FixedZone("CEST", 7200))
	archiveAt := local.UTC()
	if err := update(ContextArchived, LifecycleReasonObservedTerminalClose, local); err != nil {
		t.Fatalf("persist observed close: %v", err)
	}
	archived := testValidContext(testContextID)
	archived.State = ContextArchived
	archived.ArchivedAt = &archiveAt
	archived.Lifecycle = &LifecycleTransition{Reason: LifecycleReasonObservedTerminalClose, At: archiveAt}
	wantArchived := Registry{Version: 5, Contexts: []Context{archived}}
	assertSnapshot(wantArchived)

	// A later explicit archive is a no-op and must preserve the observed reason
	// and both timestamps after another SQLite write and fresh read.
	if err := update(ContextArchived, LifecycleReasonExplicitArchive, local.Add(time.Hour)); err != nil {
		t.Fatalf("persist archive no-op: %v", err)
	}
	assertSnapshot(wantArchived)
	if err := update(ContextActive, LifecycleReason("invalid_reason"), local.Add(2*time.Hour)); err == nil {
		t.Fatal("invalid reason update succeeded")
	}
	assertSnapshot(wantArchived)

	activateAt := archiveAt.Add(3 * time.Hour)
	if err := update(ContextActive, LifecycleReasonExplicitActivate, local.Add(3*time.Hour)); err != nil {
		t.Fatalf("persist activation: %v", err)
	}
	active := testValidContext(testContextID)
	active.Lifecycle = &LifecycleTransition{Reason: LifecycleReasonExplicitActivate, At: activateAt}
	wantActive := Registry{Version: 5, Contexts: []Context{active}}
	assertSnapshot(wantActive)
	if err := update(ContextActive, LifecycleReasonExplicitActivate, local.Add(4*time.Hour)); err != nil {
		t.Fatalf("persist activation no-op: %v", err)
	}
	assertSnapshot(wantActive)
}
