package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMetricParsers(t *testing.T) {
	times, count, err := parseCPU("cpu 100 20 30 400 50 10 5 0 80 5\ncpu0 1 2 3 4\ncpu1 1 2 3 4\n")
	if err != nil || times.total != 615 || times.idle != 450 || count != 2 {
		t.Fatal(times, count, err)
	}
	percent := cpuPercent(cpuTimes{total: 100, idle: 50}, cpuTimes{total: 200, idle: 125})
	if percent == nil || math.Abs(*percent-25) > 0.00001 {
		t.Fatal(percent)
	}
	if cpuPercent(times, times) != nil || cpuPercent(cpuTimes{total: 200}, cpuTimes{total: 100}) != nil {
		t.Fatal("counter reset should be unknown")
	}
	total, used, err := parseMemory("MemTotal: 1000 kB\nMemAvailable: 400 kB\nMemFree: 100 kB\n")
	if err != nil || total != 1024000 || used != 614400 {
		t.Fatal(total, used, err)
	}
	loads, err := parseLoad("1.20 0.50 0.20 1/320 1234\n")
	if err != nil || *loads[0] != 1.2 || *loads[1] != 0.5 || *loads[2] != 0.2 {
		t.Fatal(loads, err)
	}
	for _, text := range []string{"", "NaN 1 2", "1 -1 2", "1 +Inf 2"} {
		if _, err := parseLoad(text); err == nil {
			t.Fatal(text)
		}
	}
	if _, _, err := parseMemory("MemTotal: 1000 kB"); err == nil {
		t.Fatal("invented available memory")
	}
}

func TestSamplingAndAcknowledgementPreserveNewSamples(t *testing.T) {
	proc := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(proc, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu 10 0 10 80 0 0 0 0\ncpu0 10 0 10 80\n")
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
	write("loadavg", "1.25 0.75 0.25 1/20 9")
	r := newMetricReporter(proc)
	now := time.Now()
	if err := r.sample(now); err != nil {
		t.Fatal(err)
	}
	if r.batch()[0].CPUPercent != nil || r.batch()[0].Connected {
		t.Fatal(r.batch())
	}
	r.connected.Store(true)
	write("stat", "cpu 20 0 25 155 0 0 0 0\ncpu0 20 0 25 155\n")
	if err := r.sample(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	batch := r.batch()
	if len(batch) != 2 || *batch[1].CPUPercent != 25 || !batch[1].Connected || *batch[1].Load1 != 1.25 || batch[1].MemoryUsedBytes != 614400 {
		t.Fatal(batch)
	}
	if err := r.sample(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	r.acknowledge(batch)
	if len(r.batch()) != 1 || r.batch()[0].ObservedAt != now.Add(2*time.Minute).UnixMilli() {
		t.Fatal("ack dropped a newer sample")
	}
	if err := r.sample(now.Add(49 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(r.batch()) != 1 {
		t.Fatal("expired samples retained")
	}
}

func TestMetricsWireFrameAndAck(t *testing.T) {
	cfg := testConfig(t)
	tool := newTools(cfg.home, "/bin/sh")
	defer tool.close()
	percent, load := 25.0, 1.5
	cfg.metrics = newMetricReporter("unused")
	cfg.metrics.samples = []metricSample{{Connected: true, CPUCount: 8, CPUPercent: &percent, Load1: &load, Load5: &load, Load15: &load, MemoryTotalBytes: 1000, MemoryUsedBytes: 600, ObservedAt: time.Now().UnixMilli()}}
	server := serveSocket(t, func(c *websocket.Conn, _ *http.Request) {
		authSocket(t, c)
		for {
			frame := readFrame(t, c)
			if frame == nil {
				return
			}
			if frame["type"] == "heartbeat" {
				writeFrame(t, c, map[string]string{"type": "heartbeat_ack"})
				continue
			}
			if frame["type"] != "device_metrics" {
				t.Fatal(frame)
			}
			samples := frame["samples"].([]any)
			sample := samples[0].(map[string]any)
			if sample["cpuPercent"] != float64(25) || sample["load1"] != 1.5 || sample["memoryUsedBytes"] != float64(600) || sample["cpuCount"] != float64(8) {
				t.Fatal(sample)
			}
			// CPU/load unknown values must be JSON null, not omitted or NaN.
			data, err := json.Marshal(metricSample{})
			if err != nil || !strings.Contains(string(data), `"cpuPercent":null`) {
				t.Fatal(string(data), err)
			}
			writeFrame(t, c, map[string]any{"type": "device_metrics_ack", "batchId": frame["batchId"], "accepted": 1})
			c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Replaced by new connection"), time.Now().Add(time.Second))
			return
		}
	})
	defer server.Close()
	err := session(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), cfg, tool, 20*time.Millisecond, time.Second)
	if !errors.Is(err, errReplaced) {
		t.Fatal(err)
	}
	if len(cfg.metrics.batch()) != 0 {
		t.Fatal("acknowledged batch retained")
	}
	if cfg.metrics.connected.Load() {
		t.Fatal("disconnected reporter still online")
	}
}
