package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type sessionResult struct{ err error }

func serveSocket(t *testing.T, handler func(*websocket.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		handler(conn, r)
	}))
}
func readFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := c.ReadJSON(&frame); err != nil {
		t.Error(err)
		return nil
	}
	return frame
}
func writeFrame(t *testing.T, c *websocket.Conn, f any) {
	t.Helper()
	if err := c.WriteJSON(f); err != nil {
		t.Error(err)
	}
}
func authSocket(t *testing.T, c *websocket.Conn) {
	t.Helper()
	f := readFrame(t, c)
	if f["type"] != "auth" || f["token"] != "test-key" || f["tokenType"] != "apiKey" || f["serverUrl"] != "http://server.test" {
		t.Errorf("bad auth frame: %#v", f)
	}
	writeFrame(t, c, map[string]string{"type": "auth_success"})
}
func testConfig(t *testing.T) *config {
	t.Helper()
	return &config{server: "http://server.test", key: "test-key", home: t.TempDir(), stateDir: t.TempDir(), hostname: "phone", identity: identity{"device-id", "connection-id"}}
}

func TestGatewayProtocolAndToolRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	checked := make(chan struct{})
	server := serveSocket(t, func(c *websocket.Conn, r *http.Request) {
		defer close(checked)
		q := r.URL.Query()
		if r.URL.Path != "/edge/gw/ws" || q.Get("deviceId") != cfg.DeviceID || q.Get("connectionId") != cfg.ConnectionID || q.Get("platform") != "android" || q.Get("userId") != "user-id" || q.Get("channel") != "android" || q.Has("workspaceId") {
			t.Errorf("incorrect URL %s", r.URL)
		}
		if r.Header.Get("Authorization") != "" || q.Has("token") || q.Has("apiKey") {
			t.Error("credential leaked outside auth frame")
		}
		authSocket(t, c)
		requests := []frame{
			{Type: "system_info_request", RequestID: "info"},
			{Type: "rpc_request", RequestID: "rpc", Method: "initWorkspace"},
			{Type: "agent_run_request", OperationID: "agent"},
			{Type: "message_api_request", RequestID: "message"},
			{Type: "tunnel_open", ConnID: "http"},
			{Type: "tunnel_ws_open", ConnID: "ws"},
		}
		for _, request := range requests {
			writeFrame(t, c, request)
			response := readFrame(t, c)
			switch request.Type {
			case "system_info_request":
				if response["type"] != "system_info_response" || response["requestId"] != "info" {
					t.Fatal(response)
				}
				result := response["result"].(map[string]any)
				info := result["systemInfo"].(map[string]any)
				for _, key := range []string{"arch", "defaultShell", "homePath", "workingDirectory", "desktopPath", "documentsPath", "downloadsPath", "userDataPath"} {
					if info[key] == nil || info[key] == "" {
						t.Error("missing", key)
					}
				}
				if info["supportedTools"] != nil || info["supportedAgentRuntimes"] != nil || info["defaultShell"] != "/system/bin/sh" {
					t.Error(info)
				}
			case "rpc_request":
				if response["result"].(map[string]any)["error"] != "unsupported: initWorkspace" {
					t.Error(response)
				}
			case "agent_run_request":
				if response["type"] != "agent_run_ack" || response["operationId"] != "agent" || response["status"] != "rejected" {
					t.Error(response)
				}
			case "message_api_request":
				if response["requestId"] != "message" || response["result"].(map[string]any)["success"] != false {
					t.Error(response)
				}
			default:
				if response["type"] != request.Type+"_ack" || response["connId"] != request.ConnID || response["ok"] != false || response["error"] != "TUNNEL_DISABLED" {
					t.Error(response)
				}
			}
		}
		writeFrame(t, c, map[string]string{"type": "unknown_future_frame"})
		// Every supported file/shell call also traverses the real local WebSocket.
		requestTool := func(id, name string, args any) map[string]any {
			data, _ := json.Marshal(args)
			writeFrame(t, c, map[string]any{"type": "tool_call_request", "requestId": id, "timeout": 2000, "toolCall": toolCall{Identifier: "lobe-local-system", APIName: name, Arguments: string(data)}})
			response := readFrame(t, c)
			if response["type"] != "tool_call_response" || response["requestId"] != id {
				t.Fatal(response)
			}
			result := response["result"].(map[string]any)
			if _, ok := result["executionTimeMs"].(float64); !ok {
				t.Fatal("no execution time", result)
			}
			return result
		}
		for _, name := range []string{"writeFile", "readFile", "editFile", "listFiles", "grepContent", "globFiles"} {
			result := requestTool(name, name, map[string]any{"path": "test.txt", "content": "hello", "file_path": "test.txt", "old_string": "hello", "new_string": "world", "pattern": "test.txt", "scope": cfg.home})
			// Listing needs a directory, grep needs a content pattern.
			if name == "listFiles" {
				if result["success"] != false {
					t.Fatal("listing regular file succeeded")
				}
				result = requestTool("list-directory", name, map[string]any{"path": cfg.home})
			}
			if result["success"] != true {
				t.Fatal(result)
			}
		}
		result := requestTool("foreground", "runCommand", map[string]any{"command": "printf foreground"})
		if result["success"] != true || !strings.Contains(result["content"].(string), "foreground") {
			t.Fatal(result)
		}
		result = requestTool("background", "runCommand", map[string]any{"command": "printf background; sleep 10", "run_in_background": true})
		id := result["state"].(map[string]any)["commandId"]
		result = requestTool("poll", "getCommandOutput", map[string]any{"shell_id": id})
		if result["state"].(map[string]any)["running"] != true {
			t.Error(result)
		}
		result = requestTool("kill", "killCommand", map[string]any{"shell_id": id})
		if result["success"] != true {
			t.Error(result)
		}
		result = requestTool("poll-stopped", "getCommandOutput", map[string]any{"shell_id": id})
		if result["state"].(map[string]any)["running"] != false {
			t.Error(result)
		}
		result = requestTool("unsupported", "browser", map[string]any{})
		if result["success"] != false || result["content"] != "" {
			t.Error(result)
		}
		writeFrame(t, c, map[string]string{"type": "auth_expired"})
		result = requestTool("after-expired", "runCommand", map[string]any{"command": "true"})
		if result["success"] != true {
			t.Error("auth_expired disconnected", result)
		}
		c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Replaced by new connection"), time.Now().Add(time.Second))
	})
	defer server.Close()
	cfg.gateway = server.URL + "/edge/gw"
	endpoint, err := websocketURL(cfg, "user-id")
	if err != nil {
		t.Fatal(err)
	}
	err = session(context.Background(), endpoint, cfg, tool, time.Hour, time.Second)
	<-checked
	if !errors.Is(err, errReplaced) {
		t.Fatal(err)
	}
}

func TestHeartbeatStartsAfterAuthAndMissesReconnect(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	var heartbeats atomic.Int32
	server := serveSocket(t, func(c *websocket.Conn, _ *http.Request) {
		f := readFrame(t, c)
		if f["type"] != "auth" {
			t.Error(f)
		}
		time.Sleep(50 * time.Millisecond)
		writeFrame(t, c, map[string]string{"type": "auth_success"})
		for {
			var f map[string]any
			if err := c.ReadJSON(&f); err != nil {
				return
			}
			if f["type"] != "heartbeat" {
				t.Error(f)
			}
			heartbeats.Add(1)
		}
	})
	defer server.Close()
	err := session(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), cfg, tool, 20*time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "three heartbeat") || heartbeats.Load() != 3 {
		t.Fatalf("%v, count %d", err, heartbeats.Load())
	}
}

func TestHeartbeatAckKeepsAliveAndAuthExpiredIgnored(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	var count atomic.Int32
	server := serveSocket(t, func(c *websocket.Conn, _ *http.Request) {
		authSocket(t, c)
		writeFrame(t, c, map[string]string{"type": "auth_expired"})
		for {
			var f map[string]any
			if err := c.ReadJSON(&f); err != nil {
				return
			}
			count.Add(1)
			writeFrame(t, c, map[string]string{"type": "heartbeat_ack"})
		}
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond)
	defer cancel()
	_ = session(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), cfg, tool, 20*time.Millisecond, time.Second)
	if count.Load() < 5 {
		t.Fatalf("heartbeat stopped after %d", count.Load())
	}
}

func TestTerminalCloseDoesNotReconnect(t *testing.T) {
	for _, kind := range []string{"replaced", "auth_failed"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			tool := newTools(cfg.home, "/bin/sh")
			defer tool.close()
			var connections atomic.Int32
			server := serveSocket(t, func(c *websocket.Conn, _ *http.Request) {
				connections.Add(1)
				readFrame(t, c)
				if kind == "auth_failed" {
					writeFrame(t, c, map[string]string{"type": "auth_failed", "reason": "invalid key"})
					return
				}
				c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Replaced by new connection"), time.Now().Add(time.Second))
			})
			defer server.Close()
			cfg.gateway = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := connect(ctx, cfg, "user", tool)
			if kind == "auth_failed" && !errors.Is(err, errAuthFailed) {
				t.Fatal(err)
			}
			if kind == "replaced" && err != nil {
				t.Fatal(err)
			}
			if connections.Load() != 1 {
				t.Fatal("retried terminal failure", connections.Load())
			}
		})
	}
}

func TestReconnectPreservesIDsAndBackgroundCommand(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	command := callTool(t, tool, "runCommand", map[string]any{"command": "printf retained; sleep 10", "run_in_background": true})
	assertOK(t, command)
	id := command.State.(map[string]any)["commandId"].(string)
	var count atomic.Int32
	server := serveSocket(t, func(c *websocket.Conn, r *http.Request) {
		if r.URL.Query().Get("deviceId") != cfg.DeviceID || r.URL.Query().Get("connectionId") != cfg.ConnectionID {
			t.Error("IDs changed on reconnect")
		}
		n := count.Add(1)
		authSocket(t, c)
		if n == 1 {
			return
		} // ordinary disconnection must reconnect
		data, _ := json.Marshal(map[string]string{"shell_id": id})
		writeFrame(t, c, map[string]any{"type": "tool_call_request", "requestId": "retained", "toolCall": toolCall{Identifier: "lobe-local-system", APIName: "getCommandOutput", Arguments: string(data)}})
		result := readFrame(t, c)["result"].(map[string]any)
		if !strings.Contains(result["content"].(string), "retained") || result["state"].(map[string]any)["running"] != true {
			t.Error(result)
		}
		c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Replaced by new connection"), time.Now().Add(time.Second))
	})
	defer server.Close()
	cfg.gateway = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := connect(ctx, cfg, "user", tool); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal(count.Load())
	}
}

func TestToolTimeoutStillResponds(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	server := serveSocket(t, func(c *websocket.Conn, _ *http.Request) {
		authSocket(t, c)
		writeFrame(t, c, map[string]any{"type": "tool_call_request", "requestId": "timeout", "timeout": 80, "toolCall": toolCall{Identifier: "lobe-local-system", APIName: "runCommand", Arguments: `{"command":"sleep 10"}`}})
		start := time.Now()
		response := readFrame(t, c)
		if response["requestId"] != "timeout" || response["result"].(map[string]any)["success"] != false {
			t.Error(response)
		}
		if time.Since(start) > 200*time.Millisecond {
			t.Error("timeout reply too late")
		}
		c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Replaced by new connection"), time.Now().Add(time.Second))
	})
	defer server.Close()
	err := session(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), cfg, tool, time.Hour, time.Second)
	if !errors.Is(err, errReplaced) {
		t.Fatal(err)
	}
}

func TestWriteFailureDoesNotHideTerminalClose(t *testing.T) {
	for _, kind := range []string{"replacement", "authentication"} {
		t.Run(kind, func(t *testing.T) {
			messages := make(chan incoming, 2)
			messages <- incoming{frame: frame{Type: "heartbeat_ack"}}
			if kind == "replacement" {
				messages <- incoming{err: &websocket.CloseError{Code: 4000, Text: "Replaced by new connection"}}
			} else {
				messages <- incoming{frame: frame{Type: "auth_failed"}}
			}
			err := awaitClose(context.Background(), messages, errors.New("write: broken pipe"))
			want := errReplaced
			if kind == "authentication" {
				want = errAuthFailed
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
		})
	}
}
