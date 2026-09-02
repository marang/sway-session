// Package herdrinit initializes one empty named Herdr session through Herdr's
// public CLI without exposing general pane control to the caller.
package herdrinit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

const (
	RequiredRoles                  = 2
	supportedHerdrSnapshotProtocol = 20
	snapshotAttempts               = 24
	snapshotRetryDelay             = 250 * time.Millisecond
	shellReadinessAttempts         = 12
	shellReadinessRetryDelay       = 100 * time.Millisecond
)

type Runner interface {
	Run(context.Context, string, string, ...string) ([]byte, error)
}

type Result struct {
	ContextID   sessionstate.ContextID `json:"context_id"`
	Session     string                 `json:"session"`
	Roles       []string               `json:"roles"`
	Initialized bool                   `json:"initialized"`
	Reason      string                 `json:"reason,omitempty"`
}

func Initialize(ctx context.Context, contextValue sessionstate.Context, roles []string, runner Runner) (Result, error) {
	result := Result{ContextID: contextValue.ID, Session: contextValue.Launcher.Session, Roles: append([]string(nil), roles...)}
	if err := contextValue.Validate(); err != nil {
		return result, fmt.Errorf("validate context: %w", err)
	}
	if contextValue.State != sessionstate.ContextActive || contextValue.Launcher.Kind != sessionstate.LauncherHerdr {
		return result, errors.New("context is not an active Herdr context")
	}
	if runner == nil {
		return result, errors.New("herdr command runner is nil")
	}
	if err := ValidateRoles(roles); err != nil {
		return result, err
	}
	info, err := os.Stat(contextValue.Launcher.Cwd)
	if err != nil {
		return result, fmt.Errorf("inspect context working directory: %w", err)
	}
	if !info.IsDir() {
		return result, errors.New("context working path is not a directory")
	}

	output, err := waitForSnapshot(ctx, contextValue, runner)
	if err != nil {
		return result, fmt.Errorf("inspect Herdr session: %w", err)
	}
	rootPane, empty, err := parseEmptySession(output)
	if err != nil {
		return result, err
	}
	if !empty {
		result.Reason = "Herdr session was not proven empty and was left unchanged"
		return result, nil
	}
	ready, err := waitForReadyShell(ctx, contextValue, rootPane, runner)
	if err != nil {
		return result, err
	}
	if !ready {
		result.Reason = "Herdr root pane was not proven to be an idle project shell and was left unchanged"
		return result, nil
	}

	output, err = runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd,
		"pane", "split", rootPane, "--direction", "right", "--ratio", "0.5", "--cwd", contextValue.Launcher.Cwd, "--no-focus")
	if err != nil {
		return result, reconcileSplitFailure(ctx, contextValue, rootPane, fmt.Errorf("create Herdr shell pane: %w", err), runner)
	}
	rightPane, err := parseCreatedPane(output, rootPane)
	if err != nil {
		return result, reconcileSplitFailure(ctx, contextValue, rootPane, err, runner)
	}
	panes := []string{rootPane, rightPane}
	for index, role := range roles {
		if role == "shell" {
			continue
		}
		name := "sway-left"
		if index == 1 {
			name = "sway-right"
		}
		if _, err := runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd,
			"agent", "start", name, "--kind", role, "--pane", panes[index], "--timeout", "30000"); err != nil {
			startErr := fmt.Errorf("start %s in requested Herdr pane: %w", role, err)
			if _, rollbackErr := runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd,
				"pane", "close", panes[index]); rollbackErr != nil {
				return result, errors.Join(startErr, fmt.Errorf("roll back failed Herdr initialization: %w", rollbackErr))
			}
			return result, startErr
		}
	}
	result.Initialized = true
	return result, nil
}

func waitForReadyShell(ctx context.Context, contextValue sessionstate.Context, pane string, runner Runner) (bool, error) {
	for attempt := 0; attempt < shellReadinessAttempts; attempt++ {
		output, err := runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd,
			"pane", "process-info", "--pane", pane)
		if err != nil {
			return false, fmt.Errorf("inspect Herdr root pane process: %w", err)
		}
		ready, err := parseReadyShell(output, pane, contextValue.Launcher.Cwd)
		if err != nil || ready {
			return ready, err
		}
		if attempt+1 == shellReadinessAttempts {
			break
		}
		if err := waitForRetry(ctx, shellReadinessRetryDelay); err != nil {
			return false, err
		}
	}
	return false, nil
}

func parseReadyShell(output []byte, pane string, cwd string) (bool, error) {
	var response struct {
		Result struct {
			Type        string `json:"type"`
			ProcessInfo struct {
				PaneID                   string  `json:"pane_id"`
				ShellPID                 *uint32 `json:"shell_pid"`
				ForegroundProcessGroupID *uint32 `json:"foreground_process_group_id"`
				ForegroundProcesses      []struct {
					PID uint32  `json:"pid"`
					Cwd *string `json:"cwd"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := decodeSingleJSON(output, &response); err != nil {
		return false, fmt.Errorf("decode Herdr pane process info: %w", err)
	}
	if response.Result.Type != "pane_process_info" || response.Result.ProcessInfo.PaneID != pane {
		return false, errors.New("herdr pane process inspection returned an unexpected pane")
	}
	info := response.Result.ProcessInfo
	if info.ShellPID == nil || *info.ShellPID == 0 || info.ForegroundProcessGroupID == nil || *info.ForegroundProcessGroupID != *info.ShellPID || len(info.ForegroundProcesses) != 1 {
		return false, nil
	}
	process := info.ForegroundProcesses[0]
	return process.PID == *info.ShellPID && process.Cwd != nil && *process.Cwd == cwd, nil
}

func waitForSnapshot(ctx context.Context, contextValue sessionstate.Context, runner Runner) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < snapshotAttempts; attempt++ {
		output, err := runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd, "api", "snapshot")
		if err == nil {
			return output, nil
		}
		lastErr = err
		if attempt+1 == snapshotAttempts {
			break
		}
		if err := waitForRetry(ctx, snapshotRetryDelay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ValidateRoles accepts only the fixed Herdr layout vocabulary used by the
// typed terminal session manager. Values are logical agent kinds, never
// executable names or command fragments.
func ValidateRoles(roles []string) error {
	if len(roles) != RequiredRoles {
		return fmt.Errorf("exactly %d pane roles are required", RequiredRoles)
	}
	shells := 0
	for index, role := range roles {
		if !validRole(role) {
			return fmt.Errorf("roles[%d] must be shell or a supported Herdr agent kind", index)
		}
		if role == "shell" {
			shells++
		}
	}
	if shells != 1 {
		return errors.New("pane roles must contain exactly one shell and one supported Herdr agent kind")
	}
	return nil
}

func validRole(value string) bool {
	switch value {
	case "shell",
		"agy", "amp", "claude", "cline", "codex", "copilot", "cursor",
		"devin", "droid", "gemini", "grok", "hermes", "kilo", "kimi",
		"kiro", "maki", "mastracode", "omp", "opencode", "pi", "qodercli", "qwen":
		return true
	default:
		return false
	}
}

type snapshotPane struct {
	PaneID      string  `json:"pane_id"`
	TabID       string  `json:"tab_id"`
	WorkspaceID string  `json:"workspace_id"`
	Agent       *string `json:"agent"`
}

type snapshotLayout struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Panes       []struct {
		PaneID string `json:"pane_id"`
	} `json:"panes"`
	Splits []json.RawMessage `json:"splits"`
}

type sessionSnapshot struct {
	Version    string `json:"version"`
	Protocol   int    `json:"protocol"`
	Workspaces []struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspaces"`
	Tabs []struct {
		TabID       string `json:"tab_id"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"tabs"`
	Panes   []snapshotPane    `json:"panes"`
	Layouts []snapshotLayout  `json:"layouts"`
	Agents  []json.RawMessage `json:"agents"`
}

func decodeSessionSnapshot(output []byte) (sessionSnapshot, error) {
	var response struct {
		Result struct {
			Type     string          `json:"type"`
			Snapshot sessionSnapshot `json:"snapshot"`
		} `json:"result"`
	}
	if err := decodeSingleJSON(output, &response); err != nil {
		return sessionSnapshot{}, fmt.Errorf("decode Herdr session snapshot: %w", err)
	}
	if response.Result.Type != "session_snapshot" {
		return sessionSnapshot{}, fmt.Errorf("herdr snapshot returned unexpected result type %q", response.Result.Type)
	}
	if response.Result.Snapshot.Version == "" || response.Result.Snapshot.Protocol != supportedHerdrSnapshotProtocol {
		return sessionSnapshot{}, fmt.Errorf(
			"unsupported Herdr snapshot version %q protocol %d",
			response.Result.Snapshot.Version,
			response.Result.Snapshot.Protocol,
		)
	}
	return response.Result.Snapshot, nil
}

func parseEmptySession(output []byte) (string, bool, error) {
	snapshot, err := decodeSessionSnapshot(output)
	if err != nil {
		return "", false, err
	}
	if len(snapshot.Workspaces) != 1 || len(snapshot.Tabs) != 1 || len(snapshot.Panes) != 1 || len(snapshot.Layouts) != 1 || len(snapshot.Layouts[0].Panes) != 1 || len(snapshot.Layouts[0].Splits) != 0 || len(snapshot.Agents) != 0 {
		return "", false, nil
	}
	workspaceID := snapshot.Workspaces[0].WorkspaceID
	tabID := snapshot.Tabs[0].TabID
	pane := snapshot.Panes[0]
	layout := snapshot.Layouts[0]
	if workspaceID == "" || tabID == "" || pane.PaneID == "" || pane.Agent != nil || snapshot.Tabs[0].WorkspaceID != workspaceID || pane.WorkspaceID != workspaceID || pane.TabID != tabID || layout.WorkspaceID != workspaceID || layout.TabID != tabID || layout.Panes[0].PaneID != pane.PaneID {
		return "", false, nil
	}
	return pane.PaneID, true, nil
}

func reconcileSplitFailure(ctx context.Context, contextValue sessionstate.Context, original string, splitErr error, runner Runner) error {
	output, snapshotErr := waitForSnapshot(ctx, contextValue, runner)
	if snapshotErr != nil {
		return errors.Join(splitErr, fmt.Errorf("reconcile failed Herdr split: %w", snapshotErr))
	}
	if pane, empty, err := parseEmptySession(output); err != nil {
		return errors.Join(splitErr, fmt.Errorf("reconcile failed Herdr split: %w", err))
	} else if empty {
		if pane != original {
			return errors.Join(splitErr, errors.New("reconcile failed Herdr split: the original pane changed"))
		}
		return splitErr
	}
	created, err := parseSingleSplitSession(output, original)
	if err != nil {
		return errors.Join(splitErr, fmt.Errorf("reconcile failed Herdr split: %w", err))
	}
	if _, err := runner.Run(ctx, contextValue.Launcher.Session, contextValue.Launcher.Cwd, "pane", "close", created); err != nil {
		return errors.Join(splitErr, fmt.Errorf("roll back failed Herdr split: %w", err))
	}
	return splitErr
}

func parseSingleSplitSession(output []byte, original string) (string, error) {
	snapshot, err := decodeSessionSnapshot(output)
	if err != nil {
		return "", err
	}
	if len(snapshot.Workspaces) != 1 || len(snapshot.Tabs) != 1 || len(snapshot.Panes) != 2 || len(snapshot.Layouts) != 1 || len(snapshot.Layouts[0].Panes) != 2 || len(snapshot.Layouts[0].Splits) != 1 || len(snapshot.Agents) != 0 {
		return "", errors.New("session was not proven to contain exactly the requested split")
	}
	workspaceID := snapshot.Workspaces[0].WorkspaceID
	tabID := snapshot.Tabs[0].TabID
	layout := snapshot.Layouts[0]
	if workspaceID == "" || tabID == "" || snapshot.Tabs[0].WorkspaceID != workspaceID || layout.WorkspaceID != workspaceID || layout.TabID != tabID {
		return "", errors.New("split session identity is inconsistent")
	}
	paneIDs := make(map[string]struct{}, 2)
	created := ""
	for _, pane := range snapshot.Panes {
		if pane.PaneID == "" || pane.Agent != nil || pane.WorkspaceID != workspaceID || pane.TabID != tabID {
			return "", errors.New("split session contains an unexpected pane")
		}
		if _, duplicate := paneIDs[pane.PaneID]; duplicate {
			return "", errors.New("split session contains a duplicate pane")
		}
		paneIDs[pane.PaneID] = struct{}{}
		if pane.PaneID != original {
			created = pane.PaneID
		}
	}
	if _, found := paneIDs[original]; !found || created == "" {
		return "", errors.New("split session does not retain the original pane")
	}
	layoutIDs := make(map[string]struct{}, 2)
	for _, pane := range layout.Panes {
		if _, found := paneIDs[pane.PaneID]; !found {
			return "", errors.New("split layout references an unexpected pane")
		}
		if _, duplicate := layoutIDs[pane.PaneID]; duplicate {
			return "", errors.New("split layout contains a duplicate pane")
		}
		layoutIDs[pane.PaneID] = struct{}{}
	}
	if len(layoutIDs) != len(paneIDs) {
		return "", errors.New("split layout does not contain every pane")
	}
	return created, nil
}

func parseCreatedPane(output []byte, original string) (string, error) {
	var response struct {
		Result struct {
			Type string `json:"type"`
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := decodeSingleJSON(output, &response); err != nil {
		return "", fmt.Errorf("decode Herdr pane split: %w", err)
	}
	if response.Result.Type != "pane_info" || response.Result.Pane.PaneID == "" || response.Result.Pane.PaneID == original {
		return "", errors.New("herdr pane split returned an unexpected pane")
	}
	return response.Result.Pane.PaneID, nil
}

func decodeSingleJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
