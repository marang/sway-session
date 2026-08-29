package swayipc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CommandOutcomeUnknownError reports a mutating request whose connection
// failed after the exchange began. Callers must observe fresh compositor state
// before deciding whether another command is needed.
type CommandOutcomeUnknownError struct {
	Cause error
}

// CommandResponseInvalidError reports a response which cannot establish
// whether Sway accepted the command. Callers must re-observe before retrying.
type CommandResponseInvalidError struct {
	Cause error
}

func (err *CommandResponseInvalidError) Error() string {
	return fmt.Sprintf("invalid sway command response leaves outcome unknown: %v", err.Cause)
}

func (err *CommandResponseInvalidError) Unwrap() error {
	return err.Cause
}

func (err *CommandOutcomeUnknownError) Error() string {
	return fmt.Sprintf("sway command outcome is unknown after connection failure: %v", err.Cause)
}

func (err *CommandOutcomeUnknownError) Unwrap() error {
	return err.Cause
}

// Client reconnects once for read-only requests. Mutating requests are never
// repeated automatically because their outcome may be ambiguous.
type Client struct {
	socket string
	conn   *Conn
}

// NewClient creates a reconnecting Sway IPC request client.
func NewClient(socket string) *Client {
	return &Client{socket: socket}
}

// Close closes the current control connection, if any.
func (client *Client) Close() {
	if client == nil || client.conn == nil {
		return
	}
	_ = client.conn.Close()
	client.conn = nil
}

// Request sends one request. Known read-only requests reconnect once after a
// failed exchange; RUN_COMMAND returns CommandOutcomeUnknownError instead.
func (client *Client) Request(messageType MessageType, payload []byte) (Message, error) {
	if err := validatePayloadSize(payload); err != nil {
		return Message{}, err
	}
	attempts := 1
	if retryableReadOnlyRequest(messageType) {
		attempts = 2
	}
	var lastErr error
	for range attempts {
		if err := client.ensure(); err != nil {
			return Message{}, err
		}
		message, err := client.conn.Request(messageType, payload)
		if err == nil {
			return message, nil
		}
		lastErr = err
		client.Close()
	}
	if messageType == RunCommand {
		return Message{}, &CommandOutcomeUnknownError{Cause: lastErr}
	}
	if attempts == 1 {
		return Message{}, fmt.Errorf("sway ipc request failed: %w", lastErr)
	}
	return Message{}, fmt.Errorf("sway ipc request failed after reconnect: %w", lastErr)
}

func retryableReadOnlyRequest(messageType MessageType) bool {
	return messageType == GetTree
}

// CheckRunCommandResponse verifies that Sway acknowledged every command in a
// RUN_COMMAND response.
func CheckRunCommandResponse(message Message) error {
	if message.Type != RunCommand {
		return &CommandResponseInvalidError{Cause: fmt.Errorf("unexpected response type %d", message.Type)}
	}
	var results []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(message.Payload, &results); err != nil {
		return &CommandResponseInvalidError{Cause: fmt.Errorf("decode response: %w", err)}
	}
	if len(results) == 0 {
		return &CommandResponseInvalidError{Cause: errors.New("response contains no command result")}
	}
	for _, result := range results {
		if result.Success {
			continue
		}
		if result.Error == "" {
			result.Error = "unknown sway command error"
		}
		return errors.New(result.Error)
	}
	return nil
}

// CheckSubscribeResponse verifies that Sway accepted the event subscription.
func CheckSubscribeResponse(message Message) error {
	if message.Type != Subscribe {
		return fmt.Errorf("unexpected sway subscribe response type %d", message.Type)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return fmt.Errorf("decode sway subscribe response: %w", err)
	}
	if !result.Success {
		return errors.New("sway rejected event subscription")
	}
	return nil
}

func (client *Client) ensure() error {
	if client == nil {
		return fmt.Errorf("sway ipc client is nil")
	}
	if client.conn != nil {
		return nil
	}
	conn, err := Dial(client.socket)
	if err != nil {
		return err
	}
	client.conn = conn
	return nil
}
