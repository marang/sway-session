package session

import (
	"context"
	"encoding/json"
	"errors"
)

// DesktopApprovalReview binds a pending decision to the launch material which
// was reviewed, without creating an approval snapshot or retaining arguments.
type DesktopApprovalReview struct {
	LauncherSHA256       string
	AdministrativeSHA256 string
}

// ReviewDesktopApproval checks current catalog material using the same trust
// evidence as preparation. Ordinary system executable updates remain governed
// by their root-owned path rather than a new content pin.
func ReviewDesktopApproval(ctx context.Context, entry DesktopEntry) (DesktopApprovalReview, error) {
	if ctx == nil {
		return DesktopApprovalReview{}, errors.New("desktop approval context is nil")
	}
	if err := ctx.Err(); err != nil {
		return DesktopApprovalReview{}, err
	}
	if err := ValidateDesktopEntryForApprovalWithOptions(entry, DesktopApprovalOptions{AllowAdministrative: true}); err != nil {
		return DesktopApprovalReview{}, err
	}
	if entry.FlatpakID != "" {
		return desktopApprovalReview(nil, Launcher{Kind: LauncherFlatpak, FlatpakID: entry.FlatpakID, FlatpakInstallation: entry.FlatpakInstallation})
	}
	data, resolved, parsed, err := revalidateCatalogDesktopEntry(entry)
	if err != nil {
		return DesktopApprovalReview{}, err
	}
	administrative, err := validateDesktopApprovalOptions(parsed, DesktopApprovalOptions{AllowAdministrative: true})
	if err != nil {
		return DesktopApprovalReview{}, err
	}
	launcher := Launcher{Kind: LauncherDesktop, DesktopID: entry.ID, DesktopOrigin: entry.Origin, DesktopPath: resolved}
	if administrative {
		launcher.Administrative, err = prepareAdministrativeApproval(ctx, data, parsed)
		if err != nil {
			return DesktopApprovalReview{}, err
		}
	}
	if entry.Origin == DesktopEntryUser {
		launcher.DesktopEntrySHA256 = sha256Hex(data)
		if !administrative {
			executable, owner, digest, err := approvedDesktopExecutableContext(ctx, parsed.Exec)
			if err != nil {
				return DesktopApprovalReview{}, err
			}
			if owner != 0 {
				launcher.ApprovedExecutablePath, launcher.ApprovedExecutableSHA256 = executable, digest
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return DesktopApprovalReview{}, err
	}
	return desktopApprovalReview(data, launcher)
}

func desktopApprovalReview(data []byte, launcher Launcher) (DesktopApprovalReview, error) {
	// The immutable snapshot's path contains the context UUID. It is an output
	// of approval, not part of the reviewed source identity.
	launcher.ApprovedDesktopPath = ""
	identity := struct {
		Launcher           Launcher `json:"launcher"`
		DesktopEntrySHA256 string   `json:"desktop_entry_sha256,omitempty"`
	}{Launcher: launcher}
	if launcher.Kind == LauncherDesktop {
		identity.DesktopEntrySHA256 = sha256Hex(data)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return DesktopApprovalReview{}, err
	}
	review := DesktopApprovalReview{LauncherSHA256: sha256Hex(encoded)}
	if launcher.Administrative != nil {
		review.AdministrativeSHA256, err = launcher.Administrative.Digest()
	}
	return review, err
}
