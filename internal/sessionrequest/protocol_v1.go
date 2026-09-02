package sessionrequest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	sessionstate "github.com/marang/sway-title-animator/internal/session"
)

// responseWireV1 deliberately mirrors the original protocol-v1 response.
// Registry schema fields must not be added here without a protocol version
// change: installed clients and the long-running daemon can legitimately have
// different package versions during an upgrade.
type responseWireV1 struct {
	Version   int                `json:"version"`
	OK        bool               `json:"ok"`
	Context   *responseContextV1 `json:"context,omitempty"`
	Workspace int                `json:"workspace,omitempty"`
	Created   bool               `json:"created,omitempty"`
	Error     string             `json:"error,omitempty"`
}

type responseContextV1 struct {
	ID       sessionstate.ContextID    `json:"id"`
	Label    string                    `json:"label,omitempty"`
	Provider string                    `json:"provider,omitempty"`
	State    sessionstate.ContextState `json:"state"`
	Launcher responseLauncherV1        `json:"launcher"`
}

type responseLauncherV1 struct {
	Kind    sessionstate.LauncherKind `json:"kind"`
	Session string                    `json:"session,omitempty"`
	Cwd     string                    `json:"cwd,omitempty"`
}

func encodeResponseV1(response Response) ([]byte, error) {
	wire := responseWireV1{
		Version:   response.Version,
		OK:        response.OK,
		Workspace: response.Workspace,
		Created:   response.Created,
		Error:     response.Error,
	}
	if response.Context != nil {
		wire.Context = &responseContextV1{
			ID:       response.Context.ID,
			Label:    response.Context.Label,
			Provider: response.Context.Provider,
			State:    response.Context.State,
			Launcher: responseLauncherV1{
				Kind:    response.Context.Launcher.Kind,
				Session: response.Context.Launcher.Session,
				Cwd:     response.Context.Launcher.Cwd,
			},
		}
	}
	return json.Marshal(wire)
}

func decodeResponseV1(data []byte) (Response, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire responseWireV1
	if err := decoder.Decode(&wire); err != nil {
		return Response{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("session start response contains trailing data")
	}
	response := Response{
		Version:   wire.Version,
		OK:        wire.OK,
		Workspace: wire.Workspace,
		Created:   wire.Created,
		Error:     wire.Error,
	}
	if wire.Context != nil {
		response.Context = &sessionstate.Context{
			ID:       wire.Context.ID,
			Label:    wire.Context.Label,
			Provider: wire.Context.Provider,
			State:    wire.Context.State,
			Launcher: sessionstate.Launcher{
				Kind:     wire.Context.Launcher.Kind,
				Session:  wire.Context.Launcher.Session,
				Cwd:      wire.Context.Launcher.Cwd,
				Terminal: &sessionstate.TerminalLauncher{Adapter: sessionstate.TerminalAdapterAlacritty},
			},
		}
	}
	return response, nil
}
