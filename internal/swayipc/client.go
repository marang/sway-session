package swayipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const defaultRequestTimeout = 5 * time.Second

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
	socket         string
	conn           *Conn
	requestTimeout time.Duration
}

// NewClient creates a reconnecting Sway IPC request client.
func NewClient(socket string) *Client {
	return &Client{socket: socket, requestTimeout: defaultRequestTimeout}
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
	return client.RequestContext(context.Background(), messageType, payload)
}

// RequestContext sends one request and interrupts its Sway connection when the
// context is canceled. As with Request, read-only requests reconnect once, but
// mutating requests are never replayed after an ambiguous exchange.
func (client *Client) RequestContext(ctx context.Context, messageType MessageType, payload []byte) (Message, error) {
	if ctx == nil {
		return Message{}, errors.New("sway ipc request context is nil")
	}
	timeout := defaultRequestTimeout
	if client != nil && client.requestTimeout > 0 {
		timeout = client.requestTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.request(requestCtx, messageType, payload)
}

func (client *Client) request(ctx context.Context, messageType MessageType, payload []byte) (Message, error) {
	if err := validatePayloadSize(payload); err != nil {
		return Message{}, err
	}
	if err := ctx.Err(); err != nil {
		return Message{}, fmt.Errorf("sway ipc request canceled: %w", err)
	}
	attempts := 1
	if retryableReadOnlyRequest(messageType) {
		attempts = 2
	}
	var lastErr error
	for range attempts {
		if err := client.ensureContext(ctx); err != nil {
			return Message{}, err
		}
		message, err := requestContext(ctx, client.conn, messageType, payload)
		if err == nil {
			return message, nil
		}
		lastErr = err
		client.Close()
		if ctx.Err() != nil {
			break
		}
	}
	if messageType == RunCommand {
		return Message{}, &CommandOutcomeUnknownError{Cause: lastErr}
	}
	if attempts == 1 {
		return Message{}, fmt.Errorf("sway ipc request failed: %w", lastErr)
	}
	return Message{}, fmt.Errorf("sway ipc request failed after reconnect: %w", lastErr)
}

func requestContext(ctx context.Context, connection *Conn, messageType MessageType, payload []byte) (Message, error) {
	if ctx.Done() == nil {
		return connection.Request(messageType, payload)
	}
	completed := make(chan struct{})
	canceled := make(chan bool, 1)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
			canceled <- true
		case <-completed:
			canceled <- false
		}
	}()

	message, err := connection.Request(messageType, payload)
	close(completed)
	if <-canceled {
		if contextErr := ctx.Err(); contextErr != nil {
			return Message{}, fmt.Errorf("sway ipc request canceled: %w", contextErr)
		}
	}
	return message, err
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

// CheckSendTickResponse verifies that Sway accepted an event-stream barrier.
func CheckSendTickResponse(message Message) error {
	if message.Type != SendTick {
		return fmt.Errorf("unexpected sway send-tick response type %d", message.Type)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(message.Payload, &result); err != nil {
		return fmt.Errorf("decode sway send-tick response: %w", err)
	}
	if !result.Success {
		return errors.New("sway rejected event-stream barrier")
	}
	return nil
}

func (client *Client) ensureContext(ctx context.Context) error {
	if client == nil {
		return fmt.Errorf("sway ipc client is nil")
	}
	if client.conn != nil {
		return nil
	}
	conn, err := DialContext(ctx, client.socket)
	if err != nil {
		return err
	}
	client.conn = conn
	return nil
}
