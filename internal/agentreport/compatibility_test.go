package agentreport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOldClientUsesV2ReplyFromNewBroker(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[reject], func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "runtime", SocketFilename)
			seen := make(chan Report, 1)
			server, err := StartServer(socket, func(_ context.Context, r Report) error {
				seen <- r
				if reject {
					return errors.New("private backend detail")
				}
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			conn, err := net.Dial("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			// Exact old wire shape; an old CLI accepts only a v2 response.
			raw := `{"version":2,"context_id":"` + testContextID + `","pane_id":"work:p1","agent":"codex","agent_session_id":"thread-a"}` + "\n"
			if _, err := conn.Write([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			var result response
			if err := json.NewDecoder(conn).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.Version != 2 || result.OK == reject {
				t.Fatalf("old client incompatible reply: %+v", result)
			}
			r := <-seen
			if r.Version != 2 || r.EventOrigin != "" {
				t.Fatalf("legacy client acquired origin: %+v", r)
			}
		})
	}
}

func TestNewClientRejectsOldBrokerWithoutRetryOrDowngrade(t *testing.T) {
	for _, origin := range []string{"", "resume"} {
		t.Run("origin="+origin, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), SocketFilename)
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			seen := make(chan map[string]any, 1)
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
					done <- err
					return
				}
				var raw map[string]any
				if err := json.NewDecoder(conn).Decode(&raw); err != nil {
					done <- err
					return
				}
				seen <- raw
				// Existing v2 broker rejects v3 and responds with a generic v2 failure.
				_, err = conn.Write([]byte("{\"version\":2,\"ok\":false,\"error\":\"report rejected\"}\n"))
				done <- err
			}()
			err = Send(context.Background(), socket, Report{Version: ProtocolVersion, ContextID: testContextID, PaneID: "work:p1", Agent: "codex", AgentSessionID: "thread-b", EventOrigin: origin})
			if err == nil || !strings.Contains(err.Error(), "restart the sway-session daemon") {
				t.Fatalf("missing actionable incompatibility: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			raw := <-seen
			if raw["version"] != float64(3) {
				t.Fatalf("client downgraded: %v", raw)
			}
			if origin != "" && raw["event_origin"] != origin {
				t.Fatalf("origin lost: %v", raw)
			}
			// send returns after one exchange; it has no source-less retry path.
		})
	}
}

func TestV2CannotAcquireReplacementOrigin(t *testing.T) {
	r := Report{Version: LegacyProtocolVersion, ContextID: testContextID, PaneID: "work:p1", Agent: "codex", AgentSessionID: "thread-b", EventOrigin: "resume"}
	if err := r.Validate(); err == nil {
		t.Fatal("legacy protocol gained replacement origin")
	}
}
