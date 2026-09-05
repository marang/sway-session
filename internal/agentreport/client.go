package agentreport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
)

// ReportAgentSession is the provider-neutral hook boundary. It reads only the
// fixed input schema and managed environment, and connects only to the fixed
// v2 runtime socket.
func ReportAgentSession(ctx context.Context, input io.Reader, getenv func(string) string) error {
	report, err := ParseAgentReport(input, getenv)
	if err != nil {
		return err
	}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return err
	}
	return Send(ctx, socketPath, report)
}

func Send(ctx context.Context, socketPath string, report Report) error {
	if err := report.Validate(); err != nil {
		return err
	}
	return send(ctx, socketPath, report, ProtocolVersion)
}

// SendLegacyCodex sends exactly the stable version-1 Codex request shape.
func SendLegacyCodex(ctx context.Context, socketPath string, report LegacyCodexReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	return send(ctx, socketPath, report, 1)
}

func send(ctx context.Context, socketPath string, payload any, responseVersion int) error {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return errors.New("agent report socket must be a clean absolute path")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reportExchangeTimeout)
		defer cancel()
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect to agent session reporter: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return fmt.Errorf("bound agent report exchange: %w", err)
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode agent session report: %w", err)
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write agent session report: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(connection, maxProtocolMessage+1)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read agent report response: %w", err)
	}
	if len(line) > maxProtocolMessage {
		return fmt.Errorf("agent report response exceeds %d bytes", maxProtocolMessage)
	}
	var result response
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode agent report response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agent report response contains trailing data")
	}
	if result.Version != responseVersion {
		return fmt.Errorf("unsupported agent report response version %d", result.Version)
	}
	if !result.OK {
		return errors.New("report rejected")
	}
	return nil
}
