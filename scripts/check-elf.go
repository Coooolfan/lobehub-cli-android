//go:build ignore

// Inspect the built artifact without needing an Android NDK or host readelf.
package main

import (
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: go run ./scripts/check-elf.go lobe-device")
	}
	path := os.Args[1]
	f, err := elf.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 {
		panic("expected ARM64 ELF")
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		panic(err)
	}
	if len(libs) != 0 {
		panic(fmt.Sprintf("unexpected shared libraries: %v", libs))
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		panic(err)
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["CGO_ENABLED"] != "0" || settings["GOOS"] != "android" || settings["GOARCH"] != "arm64" {
		panic(fmt.Sprintf("unexpected build settings: %v", settings))
	}
	fmt.Printf("ELF: %s %s; GOOS=android GOARCH=arm64 CGO_ENABLED=0\n", f.Machine, f.Type)
	fmt.Printf("DT_NEEDED: %v (no shared-library dependencies)\n", libs)
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			b := make([]byte, p.Filesz)
			if _, err := p.ReadAt(b, 0); err != nil {
				panic(err)
			}
			fmt.Printf("PT_INTERP: %s (Go Android PIE default)\n", strings.TrimRight(string(b), "\x00"))
		}
	}
}
