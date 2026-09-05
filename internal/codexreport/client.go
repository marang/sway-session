package codexreport

import (
	"context"
	"io"

	"github.com/marang/sway-session/internal/agentreport"
)

// ReportCodexHook remains the installed Codex v1 hook boundary. It uses the
// generic transport's legacy mode so socket hardening is not duplicated.
func ReportCodexHook(ctx context.Context, input io.Reader, getenv func(string) string) error {
	report, err := ParseCodexHook(input, getenv)
	if err != nil {
		return err
	}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return err
	}
	return Send(ctx, socketPath, report)
}

func Send(ctx context.Context, socketPath string, report Report) error {
	return agentreport.SendLegacyCodex(ctx, socketPath, report)
}
