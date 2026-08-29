package session

import (
	"encoding/json"
	"testing"
)

const testContextID = ContextID("123e4567-e89b-12d3-a456-426614174000")

func TestContextIdentityDerivesStableGenericContracts(t *testing.T) {
	id, err := ParseContextID(string(testContextID))
	if err != nil {
		t.Fatalf("parse context ID: %v", err)
	}
	mark, err := id.Mark()
	if err != nil {
		t.Fatalf("derive mark: %v", err)
	}
	if mark != "persist:123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("unexpected mark %q", mark)
	}
	parsed, err := ParseMark(mark)
	if err != nil || parsed != id {
		t.Fatalf("parse mark: id=%q err=%v", parsed, err)
	}
	appID, err := id.AppID()
	if err != nil {
		t.Fatalf("derive application ID: %v", err)
	}
	if appID != "sway-session.123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("unexpected application ID %q", appID)
	}
	parsedAppID, err := ParseAppID(appID)
	if err != nil || parsedAppID != id {
		t.Fatalf("parse application ID: id=%q err=%v", parsedAppID, err)
	}
}

func TestContextIdentityRejectsNonCanonicalAndNilUUIDs(t *testing.T) {
	for _, value := range []string{
		"",
		"123e4567e89b12d3a456426614174000",
		"123E4567-E89B-12D3-A456-426614174000",
		"123e4567-e89b-12d3-a456-42661417400z",
		"00000000-0000-0000-0000-000000000000",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseContextID(value); err == nil {
				t.Fatalf("expected %q to be rejected", value)
			}
		})
	}
}

func TestContextIdentityRejectsInvalidManagedApplicationIDs(t *testing.T) {
	for _, value := range []string{
		"Alacritty",
		"sway-session.LAB-80",
		"sway-session.00000000-0000-0000-0000-000000000000",
	} {
		if _, err := ParseAppID(value); err == nil {
			t.Fatalf("expected application ID %q to be rejected", value)
		}
	}
}

func TestContextIDJSONRejectsInvalidValue(t *testing.T) {
	var value struct {
		ID ContextID `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id":"LAB-80"}`), &value); err == nil {
		t.Fatal("expected invalid UUID in JSON to be rejected")
	}
}
