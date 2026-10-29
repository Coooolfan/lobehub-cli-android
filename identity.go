package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func validDeviceID(value string) bool {
	if validUUID(value) {
		return true
	}
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readAndroidMachineID(ctx context.Context) (string, error) {
	if runtime.GOOS != "android" {
		return "", errors.New("Android machine identity unavailable")
	}
	for _, property := range []string{"ro.serialno", "ro.boot.serialno"} {
		commandCtx, cancel := context.WithTimeout(ctx, time.Second)
		data, err := exec.CommandContext(commandCtx, "/system/bin/getprop", property).Output()
		cancel()
		serial := strings.TrimSpace(string(data))
		if err == nil && validSerial(serial) {
			return "android-serial:" + serial, nil
		}
	}
	return "", errors.New("hardware serial unavailable; keeping persisted fallback identity")
}
func validSerial(serial string) bool {
	switch strings.ToLower(serial) {
	case "", "unknown", "null", "none", "0123456789abcdef", "0000000000000000":
		return false
	}
	return true
}
func machineDeviceID(machineID, userID string) string {
	hash := sha256.Sum256([]byte(machineID + "|" + userID + "|lobehub-device-salt"))
	return hex.EncodeToString(hash[:])[:32]
}
func (c *config) bindMachineIdentity(machineID, userID string) error {
	if machineID == "" || userID == "" {
		return errors.New("machine identity and user ID are required")
	}
	id, err := loadIdentityForDevice(c.stateDir, machineDeviceID(machineID, userID))
	if err != nil {
		return err
	}
	c.identity = id
	c.identitySource = "machine-id"
	return nil
}
func (c config) registrationIdentitySource() string {
	if c.identitySource == "machine-id" || (len(c.DeviceID) == 32 && validDeviceID(c.DeviceID)) {
		return "machine-id"
	}
	return "fallback"
}
