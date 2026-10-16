package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkEnvironmentIdentityAndValidation(t *testing.T) {
	id, err := NewEnvironmentID()
	if err != nil || id.Validate() != nil || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("new identity = %q, %v", id, err)
	}
	var decoded EnvironmentID
	if err := json.Unmarshal([]byte(`"`+string(id)+`"`), &decoded); err != nil || decoded != id {
		t.Fatalf("decoded = %q, %v", decoded, err)
	}
	for _, invalid := range []string{"", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if _, err := ParseEnvironmentID(invalid); err == nil {
			t.Errorf("accepted ID %q", invalid)
		}
	}
	valid := WorkEnvironment{ID: id, Name: "Development", Revision: 1, Members: []ContextID{testContextID}, Primary: testContextID}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", " x", "x ", "x\n", strings.Repeat("x", 257)} {
		value := valid
		value.Name = name
		if value.Validate() == nil {
			t.Errorf("accepted name %q", name)
		}
	}
	for name, mutate := range map[string]func(*WorkEnvironment){
		"revision":  func(v *WorkEnvironment) { v.Revision = 0 },
		"duplicate": func(v *WorkEnvironment) { v.Members = []ContextID{testContextID, testContextID} },
		"member":    func(v *WorkEnvironment) { v.Members = []ContextID{"invalid"} },
		"primary":   func(v *WorkEnvironment) { v.Members = nil },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if value.Validate() == nil {
				t.Fatal("accepted invalid definition")
			}
		})
	}
	empty := valid
	empty.Members, empty.Primary = nil, ""
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty editable definition: %v", err)
	}
}
