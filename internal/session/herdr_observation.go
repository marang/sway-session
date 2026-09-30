package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"
)

const herdrObservationTimeout = 3 * time.Second
const herdrObservationRPCTimeout = 750 * time.Millisecond

// ObserveSessions shares one bounded discovery across all requested names. It
// reports ephemeral evidence only: a listed directory is not proof of a usable
// saved layout, and an effective agent label is not proof of a running process.
func (manager HerdrManager) ObserveSessions(ctx context.Context, names []string, home string) map[string]TerminalSessionObservation {
	ctx, cancel := context.WithTimeout(ctx, herdrObservationTimeout)
	defer cancel()
	result := make(map[string]TerminalSessionObservation, len(names))
	valid := make([]string, 0, len(names))
	for _, name := range names {
		if _, exists := result[name]; exists {
			continue
		}
		result[name] = unknownHerdrObservation("observation_pending")
		if !validSessionName(name) || name == "default" {
			result[name] = unknownHerdrObservation("invalid_session")
			continue
		}
		valid = append(valid, name)
	}
	if len(valid) == 0 {
		return result
	}
	fail := func(reason string) map[string]TerminalSessionObservation {
		for _, name := range valid {
			result[name] = unknownHerdrObservation(reason)
		}
		return result
	}
	if ctx.Err() != nil {
		return fail("refresh_canceled")
	}
	if manager.Runner == nil || !filepath.IsAbs(manager.Executable) || ValidateTerminalCwdPath(home) != nil {
		return fail("invalid_observer")
	}
	exists, err := HerdrStateRootExists(manager.Root)
	if err != nil {
		return fail("untrusted_session_root")
	}
	if !exists {
		for _, name := range valid {
			result[name] = herdrObservation("missing", "none", "")
		}
		return result
	}
	output, err := manager.output(ctx, "session", "list", "--json")
	if ctx.Err() != nil {
		return fail("refresh_canceled")
	}
	if err != nil {
		return fail("session_list_failed")
	}
	listed, err := parseHerdrObservationList(output)
	if err != nil {
		return fail("invalid_session_list")
	}
	counts := make(map[string]int, len(listed))
	entries := make(map[string]herdrSessionInfo, len(listed))
	for _, info := range listed {
		counts[info.Name]++
		entries[info.Name] = info
	}
	live := make([]string, 0, len(valid))
	for _, name := range valid {
		if ctx.Err() != nil {
			result[name] = unknownHerdrObservation("refresh_canceled")
			continue
		}
		if counts[name] > 1 {
			result[name] = unknownHerdrObservation("duplicate_session")
			continue
		}
		info, found := entries[name]
		if !found {
			exists, err := manager.sessionPathExists(name)
			if err != nil || exists {
				result[name] = unknownHerdrObservation("session_list_inconsistent")
			} else {
				result[name] = herdrObservation("missing", "none", "")
			}
			continue
		}
		if manager.validateSessionInfo(info, name) != nil {
			result[name] = unknownHerdrObservation("untrusted_session_path")
			continue
		}
		if !info.Running {
			result[name] = herdrObservation("stopped", "none", "")
			continue
		}
		live = append(live, name)
	}
	var mu sync.Mutex
	var workers sync.WaitGroup
	jobs := make(chan string)
	for range min(4, len(live)) {
		workers.Go(func() {
			for name := range jobs {
				observation := manager.observeLiveSession(ctx, name, home)
				mu.Lock()
				result[name] = observation
				mu.Unlock()
			}
		})
	}
	for _, name := range live {
		select {
		case jobs <- name:
		case <-ctx.Done():
			mu.Lock()
			result[name] = unknownHerdrObservation("refresh_canceled")
			mu.Unlock()
		}
	}
	close(jobs)
	workers.Wait()
	return result
}

func unknownHerdrObservation(reason string) TerminalSessionObservation {
	return herdrObservation("unknown", "unknown", reason)
}

func herdrObservation(session, agent, reason string) TerminalSessionObservation {
	now := time.Now().UTC()
	return TerminalSessionObservation{SessionState: session, AgentState: agent, ObservedAt: &now, Reason: reason}
}

func parseHerdrObservationList(data []byte) ([]herdrSessionInfo, error) {
	var response struct {
		Sessions []struct {
			Name       *string `json:"name"`
			Default    *bool   `json:"default"`
			Running    *bool   `json:"running"`
			SocketPath *string `json:"socket_path"`
			SessionDir *string `json:"session_dir"`
		} `json:"sessions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || response.Sessions == nil {
		return nil, errors.New("invalid list")
	}
	infos := make([]herdrSessionInfo, 0, len(response.Sessions))
	for _, info := range response.Sessions {
		if info.Name == nil || info.Default == nil || info.Running == nil || info.SocketPath == nil || info.SessionDir == nil {
			return nil, errors.New("incomplete list entry")
		}
		infos = append(infos, herdrSessionInfo{Name: *info.Name, Default: *info.Default, Running: *info.Running, SocketPath: *info.SocketPath, SessionDir: *info.SessionDir})
	}
	return infos, nil
}

func (manager HerdrManager) observeLiveSession(ctx context.Context, name, home string) TerminalSessionObservation {
	if herdrObservationCanceled(ctx) {
		return unknownHerdrObservation("refresh_canceled")
	}
	rpcCtx, cancel := context.WithTimeout(ctx, herdrObservationRPCTimeout)
	defer cancel()
	endpoint, err := openHerdrAPIEndpoint(manager.Root, name)
	if err != nil {
		return herdrObservation("running", "unknown", "snapshot_unavailable")
	}
	defer endpoint.Close()
	data, err := endpoint.request(rpcCtx, "sway-session:observation:snapshot", "session.snapshot", struct{}{})
	if herdrObservationCanceled(ctx) {
		return unknownHerdrObservation("refresh_canceled")
	}
	if err != nil {
		reason := "snapshot_failed"
		var timeout net.Error
		if rpcCtx.Err() != nil || errors.As(err, &timeout) && timeout.Timeout() {
			reason = "snapshot_timeout"
		}
		return herdrObservation("running", "unknown", reason)
	}
	panes, err := parseHerdrLivePanes(data)
	if err != nil {
		return herdrObservation("running", "unknown", "invalid_snapshot")
	}
	observation := herdrObservation("running", "none", "")
	for _, pane := range panes {
		processData, err := endpoint.request(rpcCtx, "sway-session:observation:process", "pane.process_info", struct {
			PaneID string `json:"pane_id"`
		}{PaneID: pane.PaneID})
		if herdrObservationCanceled(ctx) {
			return unknownHerdrObservation("refresh_canceled")
		}
		state := "unknown"
		if err == nil {
			state = parseHerdrProcessActivity(processData, pane.PaneID, pane.Agent)
		}
		if state == "detected" {
			observation.AgentState = "detected"
		} else if state == "unknown" && observation.AgentState != "detected" {
			observation.AgentState = "unknown"
		}
		if err != nil {
			observation.Reason = "process_info_failed"
			var timeout net.Error
			if rpcCtx.Err() != nil || errors.As(err, &timeout) && timeout.Timeout() {
				observation.Reason = "snapshot_timeout"
			}
			break
		}
	}
	observation.Directory, err = parseHerdrPaneDirectory(data, home)
	if err != nil {
		observation.Reason = "invalid_directory"
	}
	return observation
}

func herdrObservationCanceled(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, bounded := ctx.Deadline()
	return bounded && !time.Now().Before(deadline)
}

// Herdr may retain hook identity without a process. Only current foreground
// process evidence can turn the live observation into detected or shell-only.
func parseHerdrProcessActivity(data json.RawMessage, paneID string, agent *string) string {
	var response struct {
		Type string `json:"type"`
		Info struct {
			PaneID    string  `json:"pane_id"`
			ShellPID  *uint32 `json:"shell_pid"`
			GroupID   *uint32 `json:"foreground_process_group_id"`
			Processes []struct {
				PID  uint32   `json:"pid"`
				Name string   `json:"name"`
				Argv []string `json:"argv"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if json.Unmarshal(data, &response) != nil || response.Type != "pane_process_info" || response.Info.PaneID != paneID || response.Info.ShellPID == nil || *response.Info.ShellPID == 0 || response.Info.GroupID == nil || *response.Info.GroupID == 0 || len(response.Info.Processes) == 0 {
		return "unknown"
	}
	seen := make(map[uint32]bool)
	for _, process := range response.Info.Processes {
		if process.PID == 0 || process.Name == "" || seen[process.PID] {
			return "unknown"
		}
		seen[process.PID] = true
	}
	if len(response.Info.Processes) == 1 {
		process := response.Info.Processes[0]
		if process.PID == *response.Info.ShellPID {
			switch process.Name {
			case "sh", "bash", "zsh", "dash", "fish", "ksh":
				return "none"
			}
		}
	}
	if agent != nil && ValidHerdrAgentKind(*agent) {
		for _, process := range response.Info.Processes {
			if process.Name == *agent && len(process.Argv) > 0 && filepath.Base(process.Argv[0]) == *agent {
				return "detected"
			}
		}
	}
	return "unknown"
}

type herdrLivePane struct {
	PaneID string  `json:"pane_id"`
	Agent  *string `json:"agent"`
	Status string  `json:"agent_status"`
}

// Stored associations and presentation labels deliberately do not participate.
func parseHerdrLivePanes(data json.RawMessage) ([]herdrLivePane, error) {
	var response struct {
		Type     string `json:"type"`
		Snapshot struct {
			Panes []herdrLivePane `json:"panes"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if response.Type != "session_snapshot" || response.Snapshot.Panes == nil {
		return nil, errors.New("missing live panes")
	}
	seen := make(map[string]bool, len(response.Snapshot.Panes))
	for _, pane := range response.Snapshot.Panes {
		if pane.PaneID == "" || seen[pane.PaneID] {
			return nil, errors.New("invalid pane identity")
		}
		seen[pane.PaneID] = true
		switch pane.Status {
		case "idle", "working", "blocked", "done", "unknown":
		default:
			return nil, errors.New("invalid live status")
		}
		if pane.Agent != nil && validateBoundedIdentity("agent", *pane.Agent, 256) != nil {
			return nil, errors.New("invalid live label")
		}
	}
	return response.Snapshot.Panes, nil
}
