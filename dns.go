package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// Android usually lacks resolv.conf. Without cgo Go cannot call Bionic's resolver.
// Use device DNS properties when available, otherwise public recursive resolvers.
func configureAndroidDNS() error {
	if runtime.GOOS != "android" {
		return nil
	}
	explicit := strings.TrimSpace(os.Getenv("LOBE_DEVICE_DNS"))
	servers := dnsServers(explicit)
	if explicit != "" && len(servers) == 0 {
		return errors.New("LOBE_DEVICE_DNS must contain DNS IP addresses, optionally with ports")
	}
	if explicit == "" {
		if data, err := os.ReadFile("/etc/resolv.conf"); err == nil && hasNameserver(string(data)) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		data, _ := exec.CommandContext(ctx, "/system/bin/getprop").Output()
		cancel()
		servers = propertyDNSServers(string(data))
		if len(servers) == 0 {
			servers = []string{"1.1.1.1:53", "8.8.8.8:53"}
		}
	}
	net.DefaultResolver = dnsResolver(servers)
	return nil
}

func hasNameserver(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
			return true
		}
	}
	return false
}
func dnsServers(text string) []string {
	servers := []string{}
	for _, value := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' }) {
		if net.ParseIP(value) != nil {
			servers = append(servers, net.JoinHostPort(value, "53"))
			continue
		}
		host, port, err := net.SplitHostPort(value)
		if err == nil && net.ParseIP(host) != nil && port != "" {
			servers = append(servers, net.JoinHostPort(host, port))
		}
	}
	return servers
}
func propertyDNSServers(text string) []string {
	re := regexp.MustCompile(`(?m)^\[net\.[^\]]*dns[1-4]\]: \[([^\]]+)\]$`)
	servers := []string{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		servers = append(servers, dnsServers(m[1])...)
	}
	return servers
}
func dnsResolver(servers []string) *net.Resolver {
	var next atomic.Uint64
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if len(servers) == 0 {
			return nil, errors.New("no DNS servers configured")
		}
		start := int(next.Add(1)-1) % len(servers)
		dialer := net.Dialer{Timeout: 2 * time.Second}
		var err error
		for i := 0; i < len(servers); i++ {
			var conn net.Conn
			conn, err = dialer.DialContext(ctx, network, servers[(start+i)%len(servers)])
			if err == nil {
				return conn, nil
			}
		}
		return nil, err
	}}
}
