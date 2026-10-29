package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type identity struct {
	DeviceID     string `json:"deviceId"`
	ConnectionID string `json:"connectionId"`
}
type config struct {
	server, key, gateway, stateDir, home, hostname string
	identity
	identitySource string
	metrics        *metricReporter
	client         *http.Client
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func normalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || strings.ContainsAny(raw, "?#") {
		return "", errors.New("expected http(s) URL without credentials, query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func writableHome() (string, error) {
	candidates := []string{os.Getenv("HOME"), "/root", "/data/local/tmp/lobe-device-home"}
	for _, home := range candidates {
		if home == "" || !filepath.IsAbs(home) {
			continue
		}
		if err := os.MkdirAll(home, 0700); err != nil {
			continue
		}
		f, err := os.CreateTemp(home, ".lobe-write-test-*")
		if err != nil {
			continue
		}
		f.Close()
		os.Remove(f.Name())
		if err := os.Setenv("HOME", home); err != nil {
			return "", err
		}
		return home, nil
	}
	return "", errors.New("cannot create a writable HOME")
}

func loadIdentity(dir string) (identity, error) { return loadIdentityForDevice(dir, "") }

func loadIdentityForDevice(dir, deviceID string) (identity, error) {
	var id identity
	if err := os.MkdirAll(dir, 0700); err != nil {
		return id, err
	}
	// Serialize first creation so simultaneous starts cannot invent different identities.
	lock, err := os.OpenFile(filepath.Join(dir, "identity.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return id, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return id, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(dir, "identity.json")
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &id); err != nil {
			return id, fmt.Errorf("invalid persisted identity: %w", err)
		}
		if !validDeviceID(id.DeviceID) || !validUUID(id.ConnectionID) {
			return id, errors.New("invalid persisted identity; refusing to change device identity")
		}
		if deviceID == "" || id.DeviceID == deviceID {
			return id, nil
		}
	} else {
		if !os.IsNotExist(err) {
			return id, err
		}
		id = identity{uuid(), uuid()}
	}
	if deviceID != "" {
		id.DeviceID = deviceID
	}
	data, _ = json.MarshalIndent(id, "", "  ")
	f, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return id, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return id, err
	}
	if closeErr != nil {
		return id, closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return id, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return id, err
	}
	defer d.Close()
	return id, d.Sync()
}
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func (c *config) prepare() (err error) {
	c.server, err = normalizeURL(c.server)
	if err != nil {
		return err
	}
	c.home, err = writableHome()
	if err != nil {
		return err
	}
	if c.stateDir == "" {
		c.stateDir = filepath.Join(c.home, ".lobe-device")
	}
	c.stateDir, err = filepath.Abs(c.stateDir)
	if err != nil {
		return err
	}
	c.identity, err = loadIdentity(c.stateDir)
	if err != nil {
		return err
	}
	c.hostname, err = os.Hostname()
	if err != nil || c.hostname == "" {
		c.hostname = "android"
	}
	for _, name := range []string{"Desktop", "Documents", "Downloads"} {
		if err := os.MkdirAll(filepath.Join(c.home, name), 0700); err != nil {
			return err
		}
	}
	c.client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return nil
}

func (c *config) request(ctx context.Context, method, path, auth string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, reader)
	if err != nil {
		return err
	}
	if auth == "bearer" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if auth == "key" {
		req.Header.Set("X-API-Key", c.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.New("server request failed")
	} // Do not leak credentials from transport errors.
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result)
}

func (c *config) login(ctx context.Context) (string, error) {
	var res struct {
		Data struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
		} `json:"data"`
	}
	if err := c.request(ctx, "GET", "/api/v1/users/me?includeCount=0", "bearer", nil, &res); err != nil {
		return "", fmt.Errorf("API key login failed: %w", err)
	}
	if res.Data.ID != "" {
		return res.Data.ID, nil
	}
	if res.Data.UserID != "" {
		return res.Data.UserID, nil
	}
	return "", errors.New("login response has no data.id or data.userId")
}

func (c *config) discover(ctx context.Context) (string, error) {
	var res struct {
		Result struct {
			Data struct {
				JSON struct {
					ServerConfig map[string]json.RawMessage `json:"serverConfig"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := c.request(ctx, "GET", "/trpc/lambda/config.getGlobalConfig", "", nil, &res); err != nil {
		return "", fmt.Errorf("gateway discovery failed: %w", err)
	}
	if res.Result.Data.JSON.ServerConfig == nil {
		return "", errors.New("gateway discovery response is not a global config")
	}
	value, ok := res.Result.Data.JSON.ServerConfig["deviceGatewayUrl"]
	if !ok {
		return "", errors.New("server has no deviceGatewayUrl; pass --gateway explicitly")
	}
	var endpoint string
	if err := json.Unmarshal(value, &endpoint); err != nil {
		return "", errors.New("invalid advertised gateway URL")
	}
	return normalizeURL(endpoint)
}

func (c *config) register(ctx context.Context) error {
	body := map[string]any{"json": map[string]any{"architecture": "arm64", "deviceId": c.DeviceID, "hostname": c.hostname, "identitySource": c.registrationIdentitySource(), "metadata": map[string]string{"client": "lobe-device"}, "platform": "android"}}
	var res map[string]json.RawMessage
	if err := c.request(ctx, "POST", "/trpc/lambda/device.register", "key", body, &res); err != nil {
		return err
	}
	if _, ok := res["error"]; ok {
		return errors.New("device.register returned an error")
	}
	if _, ok := res["result"]; !ok {
		return errors.New("invalid device.register response")
	}
	return nil
}
