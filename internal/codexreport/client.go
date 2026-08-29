package codexreport

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

// ReportCodexHook is the installed Codex SessionStart hook boundary. It reads
// no registry or Herdr state and connects only to the fixed runtime broker.
func ReportCodexHook(ctx context.Context, input io.Reader, getenv func(string) string) error {
	report, err := ParseCodexHook(input, getenv)
	if err != nil {
		return err
	}
	socketPath, err := DefaultSocketPath()
	if err != nil {
		return err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reportExchangeTimeout)
		defer cancel()
	}
	return Send(ctx, socketPath, report)
}

func Send(ctx context.Context, socketPath string, report Report) error {
	if err := report.Validate(); err != nil {
		return err
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return errors.New("codex report socket must be a clean absolute path")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, reportExchangeTimeout)
		defer cancel()
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect to Codex session reporter: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return fmt.Errorf("bound Codex report exchange: %w", err)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode Codex session report: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := connection.Write(encoded); err != nil {
		return fmt.Errorf("write Codex session report: %w", err)
	}

	reader := bufio.NewReader(io.LimitReader(connection, maxProtocolMessage+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read Codex report response: %w", err)
	}
	if len(line) > maxProtocolMessage {
		return fmt.Errorf("codex report response exceeds %d bytes", maxProtocolMessage)
	}
	var result response
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode Codex report response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("codex report response contains trailing data")
	}
	if result.Version != ProtocolVersion {
		return fmt.Errorf("unsupported Codex report response version %d", result.Version)
	}
	if !result.OK {
		return errors.New("report rejected")
	}
	return nil
}
