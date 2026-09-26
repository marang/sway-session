package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type HerdrDirectoryObservation struct {
	Directory string
	Ambiguous bool
}

// ObserveHerdrPaneDirectory reads one running named session without starting it.
// A display hint is available only when its panes report one non-home directory.
func ObserveHerdrPaneDirectory(ctx context.Context, paths HerdrPaths, sessionName string, home string) (HerdrDirectoryObservation, error) {
	if !validSessionName(sessionName) || sessionName == "default" {
		return HerdrDirectoryObservation{}, errors.New("invalid Herdr session name")
	}
	if err := ValidateTerminalCwdPath(home); err != nil {
		return HerdrDirectoryObservation{}, fmt.Errorf("invalid home directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	endpoint, err := openHerdrAPIEndpoint(paths.Root, sessionName)
	if err != nil {
		return HerdrDirectoryObservation{}, err
	}
	defer endpoint.Close()
	result, err := endpoint.request(ctx, "sway-session:directory:snapshot", "session.snapshot", struct{}{})
	if err != nil {
		return HerdrDirectoryObservation{}, err
	}
	return parseHerdrPaneDirectory(result, home)
}

func parseHerdrPaneDirectory(result json.RawMessage, home string) (HerdrDirectoryObservation, error) {
	var response struct {
		Type     string `json:"type"`
		Snapshot struct {
			Panes []struct {
				Cwd           string `json:"cwd"`
				ForegroundCwd string `json:"foreground_cwd"`
			} `json:"panes"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return HerdrDirectoryObservation{}, fmt.Errorf("decode Herdr session snapshot: %w", err)
	}
	if response.Type != "session_snapshot" || response.Snapshot.Panes == nil {
		return HerdrDirectoryObservation{}, errors.New("herdr did not return a session snapshot with panes")
	}
	var directory string
	for _, pane := range response.Snapshot.Panes {
		cwd := pane.ForegroundCwd
		if cwd == "" {
			cwd = pane.Cwd
		}
		if cwd == "" {
			continue
		}
		if err := ValidateTerminalCwdPath(cwd); err != nil {
			return HerdrDirectoryObservation{}, fmt.Errorf("herdr pane reported an invalid directory: %w", err)
		}
		if cwd == home || cwd == "/" {
			continue
		}
		if directory != "" && directory != cwd {
			return HerdrDirectoryObservation{Ambiguous: true}, nil
		}
		directory = cwd
	}
	return HerdrDirectoryObservation{Directory: directory}, nil
}
