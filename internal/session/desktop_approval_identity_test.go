package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopApprovalReviewMatchesPreparedEvidence(t *testing.T) {
	root := t.TempDir()
	for _, executable := range []string{directTrueExecutable(t), filepath.Join(root, "user-app")} {
		t.Run(filepath.Base(executable), func(t *testing.T) {
			if executable == filepath.Join(root, "user-app") {
				if err := os.WriteFile(executable, []byte("reviewed executable"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "org.example.App.desktop")
			if err := os.WriteFile(path, []byte("[Desktop Entry]\nType=Application\nName=Example\nExec="+executable+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			entry := parsedCatalogEntry(t, path, "org.example.App.desktop", DesktopEntryUser)
			review, err := ReviewDesktopApproval(context.Background(), entry)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareDesktopApproval(filepath.Join(root, "state"), testContextID, entry)
			if err != nil || prepared.ReviewedLauncherSHA256 != review.LauncherSHA256 || review.AdministrativeSHA256 != "" {
				t.Fatalf("review and preparation disagree: review=%+v prepared=%+v err=%v", review, prepared, err)
			}
		})
	}
}

func TestDesktopApprovalReviewBindsFlatpakRouting(t *testing.T) {
	base := DesktopEntry{ID: "org.example.App.desktop", Name: "Example", Path: "/usr/share/applications/org.example.App.desktop",
		FlatpakID: "org.example.App", FlatpakInstallation: FlatpakUser}
	review, err := ReviewDesktopApproval(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareDesktopApproval("", testContextID, base)
	if err != nil || prepared.ReviewedLauncherSHA256 != review.LauncherSHA256 {
		t.Fatalf("Flatpak review and preparation disagree: %+v err=%v", prepared, err)
	}
	for _, changed := range []DesktopEntry{
		func() DesktopEntry { value := base; value.FlatpakInstallation = FlatpakSystem; return value }(),
		func() DesktopEntry { value := base; value.FlatpakID = "org.example.Other"; return value }(),
	} {
		current, err := ReviewDesktopApproval(context.Background(), changed)
		if err != nil || current.LauncherSHA256 == review.LauncherSHA256 {
			t.Fatalf("changed Flatpak routing retained approval: %+v err=%v", changed, err)
		}
	}
}

func TestDesktopApprovalReviewCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReviewDesktopApproval(ctx, DesktopEntry{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled review continued: %v", err)
	}
	if _, _, _, err := approvedDesktopExecutableContext(ctx, "/usr/bin/true"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled executable approval continued: %v", err)
	}
}
