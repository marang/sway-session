// Package swayipc implements the bounded subset of the i3/Sway IPC protocol
// shared by the repository's commands.
package swayipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	RunCommand MessageType = 0
	Subscribe  MessageType = 2
	GetTree    MessageType = 4

	MaxPayloadSize = 64 * 1024 * 1024
)

const (
	headerSize = 14
	magic      = "i3-ipc"
)

// MessageType identifies an i3/Sway IPC request, response, or event type.
type MessageType uint32

// Message is one complete bounded IPC frame.
type Message struct {
	Type    MessageType
	Payload []byte
}

// Conn is one connected i3/Sway IPC stream.
type Conn struct {
	conn io.ReadWriteCloser
}

// Dial opens a close-on-exec Unix socket connection to Sway.
func Dial(path string) (*Conn, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return &Conn{conn: os.NewFile(uintptr(fd), path)}, nil
}

// Close closes the underlying IPC stream.
func (conn *Conn) Close() error {
	if conn == nil || conn.conn == nil {
		return nil
	}
	err := conn.conn.Close()
	conn.conn = nil
	return err
}

// Request writes one request and reads its immediate response.
func (conn *Conn) Request(messageType MessageType, payload []byte) (Message, error) {
	if conn == nil || conn.conn == nil {
		return Message{}, errors.New("sway ipc connection is closed")
	}
	if err := writeMessage(conn.conn, messageType, payload); err != nil {
		return Message{}, err
	}
	return readMessage(conn.conn)
}

// Read reads one subsequent response or subscribed event.
func (conn *Conn) Read() (Message, error) {
	if conn == nil || conn.conn == nil {
		return Message{}, errors.New("sway ipc connection is closed")
	}
	return readMessage(conn.conn)
}

func writeMessage(writer io.Writer, messageType MessageType, payload []byte) error {
	if err := validatePayloadSize(payload); err != nil {
		return err
	}
	if err := writeFull(writer, header(messageType, uint32(len(payload)))); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	return writeFull(writer, payload)
}

func validatePayloadSize(payload []byte) error {
	if uint64(len(payload)) > uint64(MaxPayloadSize) {
		return fmt.Errorf("sway ipc payload is too large: %d bytes exceeds %d", len(payload), MaxPayloadSize)
	}
	return nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func header(messageType MessageType, length uint32) []byte {
	value := make([]byte, headerSize)
	copy(value, magic)
	binary.LittleEndian.PutUint32(value[6:10], length)
	binary.LittleEndian.PutUint32(value[10:14], uint32(messageType))
	return value
}

func readMessage(reader io.Reader) (Message, error) {
	value := make([]byte, headerSize)
	if _, err := io.ReadFull(reader, value); err != nil {
		return Message{}, err
	}
	if string(value[:len(magic)]) != magic {
		return Message{}, errors.New("invalid sway ipc magic")
	}
	length := binary.LittleEndian.Uint32(value[6:10])
	if length > MaxPayloadSize {
		return Message{}, fmt.Errorf("sway ipc payload is too large: %d bytes exceeds %d", length, MaxPayloadSize)
	}
	messageType := MessageType(binary.LittleEndian.Uint32(value[10:14]))
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Message{}, err
	}
	return Message{Type: messageType, Payload: payload}, nil
}
