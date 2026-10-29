package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMachineIdentitySurvivesStateRemoval(t *testing.T) {
	c := config{stateDir: t.TempDir()}
	fallback, err := loadIdentity(c.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.bindMachineIdentity("android-serial:hardware-a", "user-a"); err != nil {
		t.Fatal(err)
	}
	first := c.identity
	if first.DeviceID == fallback.DeviceID || first.ConnectionID != fallback.ConnectionID || len(first.DeviceID) != 32 || c.registrationIdentitySource() != "machine-id" {
		t.Fatal(c.identity)
	}
	stored, err := loadIdentity(c.stateDir)
	if err != nil || stored != first {
		t.Fatal(stored, err)
	}
	fresh := config{stateDir: t.TempDir()}
	if err := fresh.bindMachineIdentity("android-serial:hardware-a", "user-a"); err != nil {
		t.Fatal(err)
	}
	if fresh.DeviceID != first.DeviceID || fresh.ConnectionID == first.ConnectionID {
		t.Fatal("machine identity not stable across independent state directories")
	}
	if first.DeviceID == machineDeviceID("android-serial:hardware-b", "user-a") || first.DeviceID == machineDeviceID("android-serial:hardware-a", "user-b") {
		t.Fatal("machine/user identities collided")
	}
	if (config{identity: first}).registrationIdentitySource() != "machine-id" {
		t.Fatal("persisted machine identity lost its source")
	}
	if (config{identity: fallback}).registrationIdentitySource() != "fallback" {
		t.Fatal("fallback mislabeled")
	}
}

func TestMachineRegistrationUsesRealSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			JSON map[string]any `json:"json"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.JSON["identitySource"] != "machine-id" {
			t.Error(body)
		}
		if body.JSON["deviceId"] != machineDeviceID("android-serial:test", "user") {
			t.Error(body)
		}
		w.Write([]byte(`{"result":{}}`))
	}))
	defer server.Close()
	c := config{server: server.URL, client: server.Client(), stateDir: t.TempDir()}
	if err := c.bindMachineIdentity("android-serial:test", "user"); err != nil {
		t.Fatal(err)
	}
	if err := c.register(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectPlaceholderSerials(t *testing.T) {
	for _, s := range []string{"", "unknown", "Unknown", "null", "none", "0123456789abcdef", "0000000000000000"} {
		if validSerial(s) {
			t.Fatal(s)
		}
	}
	if !validSerial("example-hardware-serial") {
		t.Fatal("valid serial rejected")
	}
}
