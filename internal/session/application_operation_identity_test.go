package session

import (
	"strings"
	"testing"
	"time"
)

func TestApplicationOperationReviewedLauncherDigestIsOptionalAndValidated(t *testing.T) {
	now := time.Now().UTC()
	operation := ApplicationOperation{Version: ApplicationOperationVersion, Kind: OperationReapprove, ExpiresAt: now.Add(time.Minute), Items: []ApplicationOperationItem{{
		ContextID: testContextID, ContextRevision: strings.Repeat("a", 64), DesktopID: "safe.desktop",
	}}}
	if err := operation.Validate(now); err != nil {
		t.Fatalf("legacy unbound v2 operation became unreadable: %v", err)
	}
	for _, invalid := range []string{"short", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		operation.Items[0].ReviewedLauncherSHA256 = invalid
		if err := operation.Validate(now); err == nil {
			t.Fatalf("invalid reviewed identity was accepted: %q", invalid)
		}
	}
	operation.Items[0].ReviewedLauncherSHA256 = strings.Repeat("b", 64)
	store := ApplicationOperationStore{RuntimeRoot: t.TempDir(), Now: func() time.Time { return now }}
	token, err := store.Create(operation)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := store.Consume(token)
	if err != nil || consumed.Items[0].ReviewedLauncherSHA256 != operation.Items[0].ReviewedLauncherSHA256 || consumed.Version != 2 {
		t.Fatalf("v2 operation lost its reviewed identity: %+v err=%v", consumed, err)
	}
}
