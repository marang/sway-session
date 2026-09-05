package codexreport

import (
	"context"
	"errors"
	"time"

	"github.com/marang/sway-session/internal/agentreport"
	sessionstate "github.com/marang/sway-session/internal/session"
)

// RegistryService is the v1 Codex adapter over the generic registry service.
type RegistryService struct {
	StateRoot  string
	HerdrPaths sessionstate.HerdrPaths
	Now        func() time.Time
	Report     func(context.Context, sessionstate.HerdrPaths, sessionstate.Launcher, string, string, int, time.Time) error
}

func (service RegistryService) Handle(ctx context.Context, report Report) error {
	if err := report.Validate(); err != nil {
		return err
	}
	delegate := agentreport.RegistryService{StateRoot: service.StateRoot, HerdrPaths: service.HerdrPaths, Now: service.Now}
	if service.Report != nil {
		delegate.Report = func(ctx context.Context, paths sessionstate.HerdrPaths, launcher sessionstate.Launcher, paneID string, agent string, sessionID string, peerPID int, now time.Time) error {
			if agent != "codex" {
				return errors.New("legacy Codex broker received a non-Codex agent")
			}
			return service.Report(ctx, paths, launcher, paneID, sessionID, peerPID, now)
		}
	}
	return delegate.Handle(ctx, report.Generic())
}
