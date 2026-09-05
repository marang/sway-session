package codexreport

import (
	"context"

	"github.com/marang/sway-session/internal/agentreport"
)

type Handler func(context.Context, Report) error
type Server = agentreport.Server

// StartServer preserves the v1 socket and response shape, but delegates all
// transport, peer-credential, and filesystem hardening to agentreport.
func StartServer(socketPath string, handler Handler, reportError func(error)) (*Server, error) {
	if handler == nil {
		return agentreport.StartLegacyServer(socketPath, nil, reportError)
	}
	return agentreport.StartLegacyServer(socketPath, func(ctx context.Context, report agentreport.Report) error {
		return handler(ctx, Report{Version: ProtocolVersion, ContextID: report.ContextID, PaneID: report.PaneID, CodexSessionID: report.AgentSessionID, PeerPID: report.PeerPID})
	}, reportError)
}
