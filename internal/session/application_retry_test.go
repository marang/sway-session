package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestApplicationRejectedRetryRequiresExactAttemptAndDurableAdoption(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	coordinator, err := NewApplicationRestoreCoordinator(strings.Repeat("a", 64), ApplicationSessionState{}, now,
		ApplicationRestoreOptions{AdoptionGrace: time.Second, CloseGrace: time.Second, LaunchTimeout: 10 * time.Second, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := coordinator.BeginAttempt(testContextID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := coordinator.RetryRejectedAttempt(testContextID, now.Add(time.Second)); err != nil || matched {
		t.Fatalf("stale rejection reset a newer guard: matched=%t err=%v", matched, err)
	}
	candidate, matched, err := coordinator.RetryRejectedAttempt(testContextID, now)
	if err != nil || !matched || len(candidate.Attempts) != 0 {
		t.Fatalf("rejected attempt was not prepared: %+v %t %v", candidate, matched, err)
	}
	if !reflect.DeepEqual(coordinator.State(), first) {
		t.Fatal("retry changed the coordinator before its candidate was persisted")
	}
	if _, err := coordinator.BeginAttempt(testContextID, now.Add(time.Second)); err == nil {
		t.Fatal("uncommitted retry allowed another launch")
	}
	if err := coordinator.RestoreState(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BeginAttempt(testContextID, now.Add(time.Second)); err != nil {
		t.Fatalf("adopted retry did not permit the normal launch path: %v", err)
	}
}

func TestAcceptedProcessLaunchErrorRetainsUnderlyingFailure(t *testing.T) {
	cause := errors.New("bookkeeping error")
	err := &ProcessLaunchOutcomeUnknownError{Err: cause}
	var accepted *ProcessLaunchOutcomeUnknownError
	if !errors.Is(err, cause) || !errors.As(err, &accepted) {
		t.Fatal("accepted launch cannot be distinguished from a rejected start")
	}
}
