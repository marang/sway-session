package session

import "testing"

func TestScratchpadRestoreWorkValidation(t *testing.T) {
	for _, intent := range []ScratchpadRestoreWork{{}, {Visible: true, Workspace: "98"}} {
		work := RestoreWork{Window: true, Placement: true, Scratchpad: &intent}
		if err := work.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, work := range []RestoreWork{
		{Scratchpad: &ScratchpadRestoreWork{}},
		{Placement: true, Workspace: "98", Scratchpad: &ScratchpadRestoreWork{}},
		{Placement: true, Layout: true, Scratchpad: &ScratchpadRestoreWork{}},
		{Placement: true, Scratchpad: &ScratchpadRestoreWork{Workspace: "98"}},
		{Placement: true, Scratchpad: &ScratchpadRestoreWork{Visible: true}},
		{Placement: true, Scratchpad: &ScratchpadRestoreWork{Visible: true, Workspace: "__i3_scratch"}},
	} {
		if err := work.Validate(); err == nil {
			t.Fatalf("invalid scratchpad intent accepted: %+v", work)
		}
	}
}
