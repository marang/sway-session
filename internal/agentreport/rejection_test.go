package agentreport

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerRejectsInvalidV2WireWithoutCallingHandler(t *testing.T) {
	for name, payload := range map[string][]byte{
		"unknown method":        []byte(`{"version":2,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"claude","agent_session_id":"claude:thread-1","method":"pane.send_input"}`),
		"peer PID override":     []byte(`{"version":2,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"claude","agent_session_id":"claude:thread-1","peer_pid":1}`),
		"generic v1 fields":     []byte(`{"version":1,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"claude","agent_session_id":"claude:thread-1"}`),
		"legacy v1 wire":        []byte(`{"version":1,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","codex_session_id":"01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"}`),
		"legacy field mixed in": []byte(`{"version":2,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"claude","agent_session_id":"claude:thread-1","codex_session_id":"01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee"}`),
		"invalid agent":         []byte(`{"version":2,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"not-an-agent","agent_session_id":"safe"}`),
		"invalid session ID":    []byte(`{"version":2,"context_id":"123e4567-e89b-12d3-a456-426614174000","pane_id":"work:p1","agent":"claude","agent_session_id":"-unsafe"}`),
		"oversized request":     bytes.Repeat([]byte("x"), maxProtocolMessage+1),
	} {
		t.Run(name, func(t *testing.T) {
			runtimeDir, err := os.MkdirTemp("", "agentreport-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
			socketPath := filepath.Join(runtimeDir, SocketFilename)
			var calls atomic.Int32
			server, err := StartServer(socketPath, func(context.Context, Report) error {
				calls.Add(1)
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()

			connection, err := net.Dial("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Write(append(payload, '\n')); err != nil {
				t.Fatal(err)
			}
			var result response
			if err := json.NewDecoder(connection).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 || result.Version != ProtocolVersion || result.OK || result.Error != "report rejected" {
				t.Fatalf("invalid report reached handler or exposed a non-generic response: calls=%d result=%+v", calls.Load(), result)
			}
		})
	}
}
