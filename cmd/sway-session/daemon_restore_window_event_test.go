package main

import (
	"testing"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
	"github.com/marang/sway-session/internal/swayipc"
)

func TestSessionRuntimeWindowChangesOnlyCancelSavedTargets(t *testing.T) {
	for _, change := range []string{"move", "close"} {
		for _, kind := range []string{"saved_terminal", "unmarked_saved_app", "unrelated_view", "registered_unsaved_terminal", "unknown_context", "group_with_saved_terminal", "group_with_unrelated_view", "conflicting_identity", "missing_payload"} {
			t.Run(change+"/"+kind, func(t *testing.T) {
				previous, _, app, terminal, window, now := delayedApplicationScenario(t)
				unsavedID := sessionstate.ContextID("6ba7b814-9dad-11d1-80b4-00c04fd430c8")
				registry := previous.registry
				unsaved := sessionRegistry(unsavedID).Contexts[0]
				unsaved.Launcher.Session = "unsaved-terminal"
				registry.Contexts = append(registry.Contexts, unsaved)
				runtime := &sessionRuntime{
					registry: registry, desired: previous.desired, persisted: previous.persisted,
					restoreProgress:     &sessionstate.RestoreProgress{Workspace: "98", Phase: sessionstate.RestoreBuild},
					startupApplications: map[sessionstate.ContextID]startupApplication{app.ID: {}},
				}
				identity := "org.example.Authentication"
				foreign := &Node{ID: 55, Type: "con", AppID: &identity}
				node := terminal
				wantCancelled, wantUncertain := true, false
				switch kind {
				case "unmarked_saved_app":
					node = window
				case "unrelated_view":
					node, wantCancelled = foreign, false
				case "registered_unsaved_terminal":
					node, wantCancelled = managedDaemonLeaf(t, 56, unsavedID), false
				case "unknown_context":
					node, wantCancelled = managedDaemonLeaf(t, 56, sessionstate.ContextID("6ba7b815-9dad-11d1-80b4-00c04fd430c8")), false
				case "group_with_saved_terminal":
					node = &Node{ID: 57, Type: "con", Nodes: []*Node{foreign, terminal}}
				case "group_with_unrelated_view":
					node, wantCancelled = &Node{ID: 57, Type: "con", Nodes: []*Node{foreign}}, false
				case "conflicting_identity":
					mark, _ := app.ID.Mark()
					copy := *terminal
					copy.Marks = append(append([]string{}, terminal.Marks...), mark)
					node, wantUncertain = &copy, true
				case "missing_payload":
					node, wantUncertain = nil, true
				}
				// No tree or mapping history contains this payload: a queued
				// close must still identify the already vanished saved window.
				runtime.HandleEvent(swayipc.Event{Type: swayipc.EventWindow, Change: change, Container: node}, now.Add(time.Second))
				if runtime.restoreCancelled != wantCancelled || (runtime.restoreProgress == nil) != wantCancelled {
					t.Fatalf("window event changed saved intent: cancelled=%t want=%t progress=%+v", runtime.restoreCancelled, wantCancelled, runtime.restoreProgress)
				}
				if wantCancelled {
					wantReason := "user_cancelled"
					if wantUncertain {
						wantReason = "observation_unavailable"
					}
					if runtime.restoreCancellationReason != wantReason {
						t.Fatalf("cancellation reason=%q want=%q", runtime.restoreCancellationReason, wantReason)
					}
				} else if len(runtime.startupApplications) != 1 {
					t.Fatal("unrelated window discarded delayed application intent")
				}
			})
		}
	}
}
