package herdrinit

import (
	"context"
	"errors"
	"fmt"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const startupObservationTimeout = 2 * time.Second

// A failed start can leave a live agent behind, even after Herdr clears its
// managed name. Observation adds diagnostics; it never authorizes cleanup or
// changes an unsuccessful start into a confirmed initialization.
func observeFailedAgentStart(ctx context.Context, contextValue sessionstate.Context, target string, startErr error, runner Runner) error {
	startErr = fmt.Errorf("%w; no pane was closed after the uncertain startup", startErr)
	if ctx.Err() != nil {
		return startErr
	}
	observationCtx, cancel := context.WithTimeout(ctx, startupObservationTimeout)
	defer cancel()
	output, err := runner.Run(observationCtx, contextValue.Launcher.Session, contextValue.Launcher.Cwd, "api", "snapshot")
	if err != nil {
		return errors.Join(startErr, fmt.Errorf("inspect Herdr after failed agent startup: %w", err))
	}
	observation, err := failedStartObservation(output, target)
	if err != nil {
		return errors.Join(startErr, fmt.Errorf("inspect Herdr after failed agent startup: %w", err))
	}
	return fmt.Errorf("%w; %s", startErr, observation)
}

func failedStartObservation(output []byte, target string) (string, error) {
	snapshot, err := decodeSessionSnapshot(output)
	if err != nil {
		return "", err
	}
	if snapshot.Panes == nil {
		return "", errors.New("snapshot is missing its pane list")
	}
	var found *snapshotPane
	for index := range snapshot.Panes {
		pane := &snapshot.Panes[index]
		if pane.PaneID != target {
			continue
		}
		if found != nil {
			return "", errors.New("snapshot contains duplicate target panes")
		}
		found = pane
	}
	if found == nil {
		return "target pane is absent from the latest snapshot", nil
	}
	observation := "target pane is still present"
	if found.Agent != nil && sessionstate.ValidHerdrAgentKind(*found.Agent) {
		observation += fmt.Sprintf(" with agent %s", *found.Agent)
	}
	if found.AgentStatus != nil {
		switch *found.AgentStatus {
		case "unknown", "idle", "working", "blocked", "done":
			observation += fmt.Sprintf(" (Herdr status %s)", *found.AgentStatus)
		}
	}
	return observation, nil
}
