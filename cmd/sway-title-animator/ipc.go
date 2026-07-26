package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	ipcRunCommand = 0
	ipcSubscribe  = 2
	ipcGetTree    = 4
	maxIPCPayload = 64 * 1024 * 1024
)

type IPC struct {
	socket string
	conn   io.ReadWriteCloser
}

func (ipc *IPC) Close() {
	if ipc.conn != nil {
		_ = ipc.conn.Close()
		ipc.conn = nil
	}
}

func (ipc *IPC) ensure() error {
	if ipc.conn != nil {
		return nil
	}
	conn, err := dialUnixSocket(ipc.socket)
	if err != nil {
		return err
	}
	ipc.conn = conn
	return nil
}

func (ipc *IPC) Request(messageType uint32, payload string) ([]byte, uint32, error) {
	var lastErr error
	for range 2 {
		if err := ipc.ensure(); err != nil {
			return nil, 0, err
		}
		body, responseType, err := ipc.requestOnce(messageType, payload)
		if err == nil {
			return body, responseType, nil
		}
		lastErr = err
		ipc.Close()
	}
	return nil, 0, fmt.Errorf("ipc request failed after reconnect: %w", lastErr)
}

func (ipc *IPC) requestOnce(messageType uint32, payload string) ([]byte, uint32, error) {
	if err := writeFull(ipc.conn, ipcHeader(messageType, len([]byte(payload)))); err != nil {
		return nil, 0, err
	}
	if payload != "" {
		if err := writeFull(ipc.conn, []byte(payload)); err != nil {
			return nil, 0, err
		}
	}
	return readIPCMessage(ipc.conn)
}

func dialUnixSocket(path string) (io.ReadWriteCloser, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
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

func ipcHeader(messageType uint32, length int) []byte {
	header := make([]byte, 14)
	copy(header, []byte("i3-ipc"))
	binary.LittleEndian.PutUint32(header[6:10], uint32(length))
	binary.LittleEndian.PutUint32(header[10:14], messageType)
	return header
}

func readIPCMessage(reader io.Reader) ([]byte, uint32, error) {
	header := make([]byte, 14)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, 0, err
	}
	if string(header[:6]) != "i3-ipc" {
		return nil, 0, errors.New("invalid ipc magic")
	}
	length := binary.LittleEndian.Uint32(header[6:10])
	messageType := binary.LittleEndian.Uint32(header[10:14])
	if length > maxIPCPayload {
		return nil, 0, fmt.Errorf("ipc payload is too large: %d bytes exceeds %d", length, maxIPCPayload)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, 0, err
	}
	return body, messageType, nil
}
