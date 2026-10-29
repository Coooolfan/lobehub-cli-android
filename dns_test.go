package main

import (
	"context"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestDNSConfiguration(t *testing.T) {
	want := []string{"192.168.1.1:53", "[2001:db8::1]:53", "127.0.0.1:5353", "[::1]:5353"}
	got := dnsServers("192.168.1.1,2001:db8::1;127.0.0.1:5353 [::1]:5353,invalid")
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	got = propertyDNSServers("[net.dns1]: [192.168.1.1]\n[net.wlan0.dns2]: [2001:db8::1]\n[unrelated]: [8.8.8.8]")
	if !reflect.DeepEqual(got, want[:2]) {
		t.Fatal(got)
	}
	if hasNameserver("# nameserver 1.1.1.1") || !hasNameserver("nameserver 192.168.1.1\n") {
		t.Fatal("incorrect resolv.conf detection")
	}
}

func TestResolverUsesSelectedDNS(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		data := make([]byte, 512)
		for {
			n, peer, err := server.ReadFrom(data)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			// Preserve the question, excluding Go's optional EDNS0 additional record.
			pos := 12
			for pos < n && data[pos] != 0 {
				pos += 1 + int(data[pos])
			}
			if pos+5 > n {
				continue
			}
			pos++
			query := append([]byte(nil), data[:pos+4]...)
			qtype := binary.BigEndian.Uint16(data[pos : pos+2])
			binary.BigEndian.PutUint16(query[10:12], 0)
			// Reply to the original DNS question with an authoritative IPv4 A record.
			binary.BigEndian.PutUint16(query[2:4], 0x8180)
			binary.BigEndian.PutUint16(query[6:8], 0)
			if qtype == 1 {
				binary.BigEndian.PutUint16(query[6:8], 1)
				query = append(query, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 9)
			}
			server.WriteTo(query, peer)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ips, err := dnsResolver([]string{server.LocalAddr().String()}).LookupIP(ctx, "ip4", "device-test.invalid")
	if err != nil || len(ips) != 1 || ips[0].String() != "192.0.2.9" {
		t.Fatalf("%v %v", ips, err)
	}
	server.Close()
	<-stopped
}
