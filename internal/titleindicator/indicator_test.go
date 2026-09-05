package titleindicator_test

import (
	"testing"

	"github.com/marang/sway-session/internal/titleindicator"
)

func TestApplicationStateRoundTripsThroughOneHiddenSwayMark(t *testing.T) {
	states := []titleindicator.State{
		titleindicator.Unregistered,
		titleindicator.Pending,
		titleindicator.Registered,
		titleindicator.Pinned,
	}

	for _, state := range states {
		mark, err := titleindicator.Mark(state, 42)
		if err != nil {
			t.Fatalf("mark %q: %v", state, err)
		}
		if mark == "" || mark[0] != '_' {
			t.Fatalf("mark %q is not hidden from ordinary title badges", mark)
		}
		got, ok := titleindicator.FromMarks([]string{"ordinary", mark}, 42)
		if !ok || got != state {
			t.Fatalf("marks decoded as (%q, %v), want (%q, true)", got, ok, state)
		}
	}
}

func TestApplicationStateMarksAreUniquePerContainer(t *testing.T) {
	first, err := titleindicator.Mark(titleindicator.Registered, 41)
	if err != nil {
		t.Fatal(err)
	}
	second, err := titleindicator.Mark(titleindicator.Registered, 42)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two containers received the same globally unique Sway mark %q", first)
	}
	if state, ok := titleindicator.FromMarks([]string{first}, 42); ok || state != "" {
		t.Fatalf("container 42 accepted container 41 mark as (%q, %v)", state, ok)
	}
}
