package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type metricSample struct {
	Connected        bool     `json:"connected"`
	CPUCount         int      `json:"cpuCount"`
	CPUPercent       *float64 `json:"cpuPercent"`
	Load1            *float64 `json:"load1"`
	Load5            *float64 `json:"load5"`
	Load15           *float64 `json:"load15"`
	MemoryTotalBytes int64    `json:"memoryTotalBytes"`
	MemoryUsedBytes  int64    `json:"memoryUsedBytes"`
	ObservedAt       int64    `json:"observedAt"`
}
type cpuTimes struct{ total, idle uint64 }

type metricReporter struct {
	proc      string
	connected atomic.Bool
	mu        sync.Mutex
	samples   []metricSample
	previous  *cpuTimes
}

func newMetricReporter(proc string) *metricReporter { return &metricReporter{proc: proc} }

func parseCPU(text string) (cpuTimes, int, error) {
	var times cpuTimes
	count := 0
	found := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			if len(fields) < 5 {
				return times, 0, errors.New("invalid aggregate CPU counters")
			}
			// guest/guest_nice are already included in user/nice; do not double count them.
			for i := 1; i < min(len(fields), 9); i++ {
				value, err := strconv.ParseUint(fields[i], 10, 64)
				if err != nil {
					return times, 0, err
				}
				times.total += value
				if i == 4 || i == 5 {
					times.idle += value
				}
			}
			found = true
		} else if strings.HasPrefix(fields[0], "cpu") {
			if _, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err == nil {
				count++
			}
		}
	}
	if !found || count < 1 || count > 4096 {
		return times, 0, errors.New("missing CPU counters")
	}
	return times, count, nil
}
func cpuPercent(previous, next cpuTimes) *float64 {
	if next.total <= previous.total || next.idle < previous.idle {
		return nil
	}
	total := float64(next.total - previous.total)
	value := math.Max(0, math.Min(100, (1-float64(next.idle-previous.idle)/total)*100))
	return &value
}
func parseMemory(text string) (total, used int64, err error) {
	values := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		n, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil || n < 0 || n > (1<<63-1)/1024 {
			continue
		}
		values[strings.TrimSuffix(fields[0], ":")] = n * 1024
	}
	total, ok := values["MemTotal"]
	if !ok || total <= 0 {
		return 0, 0, errors.New("missing MemTotal")
	}
	available, ok := values["MemAvailable"]
	if !ok {
		return 0, 0, errors.New("missing MemAvailable")
	}
	return total, max(0, total-min(total, available)), nil
}
func parseLoad(text string) ([3]*float64, error) {
	var result [3]*float64
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return result, errors.New("missing load averages")
	}
	for i := range result {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return [3]*float64{}, errors.New("invalid load average")
		}
		result[i] = &value
	}
	return result, nil
}
func (r *metricReporter) sample(now time.Time) error {
	stat, err := os.ReadFile(filepath.Join(r.proc, "stat"))
	if err != nil {
		return err
	}
	times, count, err := parseCPU(string(stat))
	if err != nil {
		return err
	}
	mem, err := os.ReadFile(filepath.Join(r.proc, "meminfo"))
	if err != nil {
		return err
	}
	total, used, err := parseMemory(string(mem))
	if err != nil {
		return err
	}
	var loads [3]*float64
	if load, err := os.ReadFile(filepath.Join(r.proc, "loadavg")); err == nil {
		loads, _ = parseLoad(string(load))
	}
	var percent *float64
	if r.previous != nil {
		percent = cpuPercent(*r.previous, times)
	}
	r.previous = &times
	sample := metricSample{Connected: r.connected.Load(), CPUCount: count, CPUPercent: percent, Load1: loads[0], Load5: loads[1], Load15: loads[2], MemoryTotalBytes: total, MemoryUsedBytes: used, ObservedAt: now.UnixMilli()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, sample)
	cutoff := now.Add(-48 * time.Hour).UnixMilli()
	first := 0
	for first < len(r.samples) && (r.samples[first].ObservedAt < cutoff || len(r.samples)-first > 2882) {
		first++
	}
	if first > 0 {
		r.samples = append([]metricSample(nil), r.samples[first:]...)
	}
	return nil
}
func (r *metricReporter) run(ctx context.Context) {
	// The first reading has no CPU delta. A second reading makes CPU visible promptly.
	if err := r.sample(time.Now()); err != nil {
		log.Print("device metrics unavailable: ", err)
	}
	initial := time.NewTimer(time.Second)
	defer initial.Stop()
	select {
	case <-ctx.Done():
		return
	case <-initial.C:
	}
	if err := r.sample(time.Now()); err != nil {
		log.Print("device metrics unavailable: ", err)
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := r.sample(now); err != nil {
				log.Print("device metrics unavailable: ", err)
			}
		}
	}
}
func (r *metricReporter) batch() []metricSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]metricSample(nil), r.samples[:min(500, len(r.samples))]...)
}
func (r *metricReporter) acknowledge(samples []metricSample) {
	if len(samples) == 0 {
		return
	}
	last := samples[len(samples)-1].ObservedAt
	r.mu.Lock()
	defer r.mu.Unlock()
	first := 0
	for first < len(r.samples) && r.samples[first].ObservedAt <= last {
		first++
	}
	r.samples = append([]metricSample(nil), r.samples[first:]...)
}
func (s metricSample) summary() string {
	cpu := "unavailable"
	if s.CPUPercent != nil {
		cpu = fmt.Sprintf("%.1f%%", *s.CPUPercent)
	}
	memory := float64(s.MemoryUsedBytes) * 100 / float64(s.MemoryTotalBytes)
	return fmt.Sprintf("cpu=%s memory=%.1f%%", cpu, memory)
}
