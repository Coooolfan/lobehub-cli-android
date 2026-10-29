package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIdentityConcurrentAndRestart(t *testing.T) {
	dir := t.TempDir()
	const count = 12
	ids := make(chan identity, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := loadIdentity(dir)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	first, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	for id := range ids {
		if id != first {
			t.Fatalf("identity changed: %v vs %v", id, first)
		}
	}
	if !validUUID(first.DeviceID) || !validUUID(first.ConnectionID) {
		t.Fatal(first)
	}
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIdentity(dir); err == nil {
		t.Fatal("silently replaced corrupt identity")
	}
}

func TestWritableHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "new-home")
	t.Setenv("HOME", home)
	got, err := writableHome()
	if err != nil || got != home || os.Getenv("HOME") != home {
		t.Fatalf("%s %v", got, err)
	}
}

func TestHTTPLoginDiscoveryRegistration(t *testing.T) {
	for _, idField := range []string{"id", "userId"} {
		t.Run(idField, func(t *testing.T) {
			var host string
			key := "test-api-key"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/users/me":
					if r.Header.Get("Authorization") != "Bearer "+key || r.URL.Query().Get("includeCount") != "0" {
						t.Error("incorrect login")
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{idField: "user-1"}})
				case "/trpc/lambda/config.getGlobalConfig":
					if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
						t.Error("discovery leaked API key")
					}
					json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"data": map[string]any{"json": map[string]any{"serverConfig": map[string]string{"deviceGatewayUrl": host + "/edge/gw/"}}}}})
				case "/trpc/lambda/device.register":
					if r.Method != "POST" || r.Header.Get("X-API-Key") != key || r.Header.Get("Content-Type") != "application/json" {
						t.Error("incorrect registration headers")
					}
					var body struct {
						JSON map[string]any `json:"json"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.JSON["deviceId"] != "device" || body.JSON["platform"] != "android" || body.JSON["architecture"] != "arm64" || body.JSON["identitySource"] != "fallback" {
						t.Error(body)
					}
					if _, ok := body.JSON["workspaceId"]; ok {
						t.Error("workspace scope leaked")
					}
					w.Write([]byte(`{"result":{"data":{"json":{}}}}`))
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			host = server.URL
			c := config{server: host, key: key, hostname: "phone", identity: identity{DeviceID: "device"}, client: server.Client()}
			ctx := context.Background()
			id, err := c.login(ctx)
			if err != nil || id != "user-1" {
				t.Fatalf("%s %v", id, err)
			}
			gateway, err := c.discover(ctx)
			if err != nil || gateway != host+"/edge/gw" {
				t.Fatalf("%s %v", gateway, err)
			}
			if err := c.register(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscoveryFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"result":{"data":{"json":{"serverConfig":{}}}}}`, `{"result":{"data":{"json":{"serverConfig":{"deviceGatewayUrl":null}}}}}`, `{"result":{"data":{"json":{"serverConfig":{"deviceGatewayUrl":"https://u:p@example.com"}}}}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		c := config{server: server.URL, client: server.Client()}
		if endpoint, err := c.discover(context.Background()); err == nil {
			t.Errorf("accepted malformed config %s: %s", body, endpoint)
		}
		server.Close()
	}
}

func TestLoginFailureAndRegistrationForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	c := config{server: server.URL, client: server.Client()}
	if _, err := c.login(context.Background()); err == nil {
		t.Fatal("login accepted forbidden")
	}
	if err := c.register(context.Background()); err == nil {
		t.Fatal("registration accepted forbidden")
	}
}

func TestURLValidationAndPathPrefix(t *testing.T) {
	c := config{gateway: "https://example.com/edge/gw/", hostname: "phone & tablet", identity: identity{"d", "c"}}
	got, err := websocketURL(&c, "u")
	want := "wss://example.com/edge/gw/ws?channel=android&connectionId=c&deviceId=d&hostname=phone+%26+tablet&platform=android&userId=u"
	if err != nil || got != want {
		t.Fatalf("%s %v", got, err)
	}
	for _, raw := range []string{"ws://example.com", "https://user@example.com", "https://example.com?", "https://example.com#", "https:///foo"} {
		if _, err := normalizeURL(raw); err == nil {
			t.Error(raw)
		}
	}
}

func TestPrepareCreatesWritablePaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	c := config{server: "https://example.com/"}
	if err := c.prepare(); err != nil {
		t.Fatal(err)
	}
	if c.home != home || c.server != "https://example.com" || c.stateDir != filepath.Join(home, ".lobe-device") {
		t.Fatal(c)
	}
	for _, path := range []string{c.stateDir, filepath.Join(home, "Desktop"), filepath.Join(home, "Documents"), filepath.Join(home, "Downloads")} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("%s %v", path, err)
		}
	}
}
