package codexreport

import (
	"context"
	"errors"
	"fmt"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

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
	now := time.Now
	if service.Now != nil {
		now = service.Now
	}
	reportHerdr := sessionstate.ReportHerdrCodexSession
	if service.Report != nil {
		reportHerdr = service.Report
	}
	registry := sessionstate.Registry{}
	if err := sessionstate.RegistryFile(service.StateRoot).LoadSnapshotInto(&registry); err != nil {
		return fmt.Errorf("load context registry snapshot: %w", err)
	}
	for _, candidate := range registry.Contexts {
		if candidate.ID != report.ContextID {
			continue
		}
		if candidate.Launcher.Kind != sessionstate.LauncherHerdr {
			return errors.New("registered context does not use Herdr")
		}
		if err := reportHerdr(ctx, service.HerdrPaths, candidate.Launcher, report.PaneID, report.CodexSessionID, report.PeerPID, now()); err != nil {
			return fmt.Errorf("record Codex session association: %w", err)
		}
		return nil
	}
	return errors.New("reported context is not registered")
}
