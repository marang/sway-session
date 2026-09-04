package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplicationSessionStoreRejectsAttemptForTerminalContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	terminal := validRegistry().Contexts[0]
	if err := RegistryStoreFor(root).Save(Registry{
		Version: ContextsSchemaVersion, Contexts: []Context{terminal},
	}); err != nil {
		t.Fatal(err)
	}
	state := ApplicationSessionState{
		Version:      ApplicationSessionSchemaVersion,
		CompositorID: strings.Repeat("a", 64),
		Attempts: []ApplicationLaunchAttempt{{
			ContextID: terminal.ID,
			StartedAt: time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC),
		}},
	}

	err := ApplicationSessionStoreFor(root).Save(state)
	if err == nil || !strings.Contains(err.Error(), "not a desktop application context") {
		t.Fatalf("terminal context application attempt error = %v", err)
	}
	var loaded ApplicationSessionState
	if err := ApplicationSessionStoreFor(root).LoadInto(&loaded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected application attempt left state behind: state=%+v err=%v", loaded, err)
	}
}

func TestTerminalActivityStoreRejectsDesktopApplicationContext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sway-session")
	application := flatpakApplicationContext("org.example.App", "org.example.App")
	application.ID = "22222222-2222-4222-8222-222222222222"
	if err := RegistryStoreFor(root).Save(Registry{
		Version: ContextsSchemaVersion, Contexts: []Context{application},
	}); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)
	state := TerminalActivityState{
		Version: TerminalActivitySchemaVersion,
		Terminals: []TerminalActivity{{
			ContextID: application.ID,
			CreatedAt: &createdAt,
		}},
	}

	err := TerminalActivityStoreFor(root).Save(state)
	if err == nil || !strings.Contains(err.Error(), "not a typed terminal context") {
		t.Fatalf("desktop context terminal activity error = %v", err)
	}
	loaded, err := ReadTerminalActivitySnapshot(root)
	if err != nil || len(loaded.Terminals) != 0 {
		t.Fatalf("rejected terminal activity left state behind: state=%+v err=%v", loaded, err)
	}
}
