package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const herdrRemoteCatalogTimeout = 3 * time.Second

// ResolveRemoteMachine resolves a saved machine ID or one unique, case-sensitive
// label. The returned route pins the immutable ID and its current target/session;
// it never changes Herdr's desired selected machine or the saved catalog.
func (manager HerdrManager) ResolveRemoteMachine(ctx context.Context, selector string) (TerminalRemoteBinding, error) {
	if strings.TrimSpace(selector) == "" || containsControl(selector) {
		return TerminalRemoteBinding{}, errors.New("remote machine selector is empty or contains control characters")
	}
	profiles, err := manager.remoteMachineProfiles(ctx)
	if err != nil {
		return TerminalRemoteBinding{}, err
	}
	// IDs always take precedence over a coincidentally equal display label.
	for _, profile := range profiles {
		if profile.binding.MachineID == selector {
			return enabledRemoteMachine(profile)
		}
	}
	var matched *herdrRemoteProfile
	for i := range profiles {
		if profiles[i].label == selector {
			if matched != nil {
				return TerminalRemoteBinding{}, errors.New("remote machine label is ambiguous; use its exact profile ID")
			}
			matched = &profiles[i]
		}
	}
	if matched == nil {
		return TerminalRemoteBinding{}, errors.New("remote machine is missing; use herdr machine list to find its exact profile ID")
	}
	return enabledRemoteMachine(*matched)
}

// ValidateRemoteBinding checks saved profile identity without contacting the
// remote host. Direct bindings need no catalog; readiness remains unknown.
func (manager HerdrManager) ValidateRemoteBinding(ctx context.Context, binding TerminalRemoteBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.MachineID == "" {
		return nil
	}
	current, err := manager.ResolveRemoteMachine(ctx, binding.MachineID)
	if err != nil {
		return err
	}
	if current != binding {
		return errors.New("saved remote machine target or session changed; explicitly register a new binding")
	}
	return nil
}

type herdrRemoteProfile struct {
	binding TerminalRemoteBinding
	label   string
	enabled bool
}

func enabledRemoteMachine(profile herdrRemoteProfile) (TerminalRemoteBinding, error) {
	if !profile.enabled {
		return TerminalRemoteBinding{}, errors.New("saved remote machine is disabled")
	}
	return profile.binding, nil
}

func (manager HerdrManager) remoteMachineProfiles(ctx context.Context) ([]herdrRemoteProfile, error) {
	if ctx == nil {
		return nil, errors.New("remote machine context is nil")
	}
	if manager.Runner == nil || !filepath.IsAbs(manager.Executable) {
		return nil, errors.New("remote machine command dependency is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, herdrRemoteCatalogTimeout)
	defer cancel()
	output, err := manager.output(ctx, "machine", "list", "--json")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		// Do not propagate combined output: it can contain SSH targets or
		// arbitrary wrapper diagnostics. This command does not contact hosts.
		return nil, errors.New("herdr saved machine catalog could not be read")
	}
	return parseRemoteMachineProfiles(output)
}

func parseRemoteMachineProfiles(data []byte) ([]herdrRemoteProfile, error) {
	var rows []struct {
		ID       *string `json:"id"`
		Label    *string `json:"label"`
		Target   *string `json:"target"`
		Session  *string `json:"session"`
		Enabled  *bool   `json:"enabled"`
		Selected *bool   `json:"selected"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&rows); err != nil || rows == nil {
		return nil, errors.New("herdr saved machine catalog is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("herdr saved machine catalog contains trailing data")
	}
	profiles := make([]herdrRemoteProfile, 0, len(rows))
	ids := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == nil || row.Label == nil || row.Target == nil || row.Session == nil || row.Enabled == nil || row.Selected == nil || strings.TrimSpace(*row.Label) == "" || containsControl(*row.Label) {
			return nil, errors.New("herdr saved machine catalog contains an incomplete profile")
		}
		binding := TerminalRemoteBinding{MachineID: *row.ID, Target: *row.Target, Session: *row.Session}
		if binding.MachineID == "" || binding.Validate() != nil {
			return nil, errors.New("herdr saved machine catalog contains an invalid profile")
		}
		if ids[binding.MachineID] {
			return nil, errors.New("herdr saved machine catalog contains duplicate profile IDs")
		}
		ids[binding.MachineID] = true
		profiles = append(profiles, herdrRemoteProfile{binding: binding, label: *row.Label, enabled: *row.Enabled})
	}
	return profiles, nil
}
