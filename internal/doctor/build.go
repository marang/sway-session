package doctor

import (
	"fmt"

	"github.com/marang/sway-session/internal/buildmetadata"
)

func buildEvidence(label string, metadata buildmetadata.Metadata) []string {
	version, commit := metadata.Version, metadata.Commit
	if version == "" {
		version = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	modified := fmt.Sprint(metadata.Modified)
	if commit == "unknown" {
		modified = "unknown"
	}
	evidence := []string{fmt.Sprintf("%s build: version=%s commit=%s modified=%s", label, version, commit, modified)}
	if version == "unknown" || commit == "unknown" {
		evidence = append(evidence, label+" build metadata is incomplete; older or unstamped binaries may not retain a product version or source commit.")
	}
	return evidence
}
