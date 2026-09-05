package sessionrequest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	sessionstate "github.com/marang/sway-session/internal/session"
)

const exchangeTimeout = 15 * time.Second

func Send(ctx context.Context, socketPath string, request Request) (Response, error) {
	if err := request.Validate(); err != nil {
		return Response{}, err
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return Response{}, errors.New("session request socket must be a clean absolute path")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, exchangeTimeout)
		defer cancel()
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("connect to session start broker: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return Response{}, fmt.Errorf("bound session start exchange: %w", err)
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("encode session start request: %w", err)
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return Response{}, fmt.Errorf("write session start request: %w", err)
	}
	reader := bufio.NewReader(io.LimitReader(connection, maxProtocolMessage+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Response{}, fmt.Errorf("read session start response: %w", err)
	}
	if len(line) > maxProtocolMessage {
		return Response{}, fmt.Errorf("session start response exceeds %d bytes", maxProtocolMessage)
	}
	response, err := decodeResponseV1(line)
	if err != nil {
		return Response{}, fmt.Errorf("decode session start response: %w", err)
	}
	if response.Version != ProtocolVersion {
		return Response{}, fmt.Errorf("unsupported session start response version %d", response.Version)
	}
	if !response.OK {
		return Response{}, errors.New("session start request rejected")
	}
	if response.Context == nil {
		return Response{}, errors.New("session start response has no context")
	}
	if err := response.Context.Validate(); err != nil {
		return Response{}, fmt.Errorf("invalid context in session start response: %w", err)
	}
	if response.Context.State != sessionstate.ContextActive || response.Workspace != request.Workspace || response.Context.Label != request.Label || response.Context.Provider != request.Provider || response.Context.Launcher.Session != request.Session || response.Context.Launcher.Cwd != request.Cwd {
		return Response{}, errors.New("session start response does not match request")
	}
	return response, nil
}
