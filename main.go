package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.server, "server", "", "LobeHub server root URL (required)")
	flag.StringVar(&cfg.key, "api-key", "", "Personal API key (required)")
	flag.StringVar(&cfg.gateway, "gateway", "", "Gateway root URL (default: server discovery)")
	flag.StringVar(&cfg.stateDir, "state-dir", "", "Identity directory (default: $HOME/.lobe-device)")
	flag.Parse()
	if cfg.server == "" || cfg.key == "" {
		return errors.New("--server and --api-key are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := configureAndroidDNS(); err != nil {
		return err
	}
	if err := cfg.prepare(); err != nil {
		return err
	}
	user, err := cfg.login(ctx)
	if err != nil {
		return err
	}
	if machineID, machineErr := readAndroidMachineID(ctx); machineErr == nil {
		if err := cfg.bindMachineIdentity(machineID, user); err != nil {
			return err
		}
	} else {
		log.Print(machineErr)
	}
	log.Print("device identity source: ", cfg.registrationIdentitySource())
	if cfg.gateway == "" {
		cfg.gateway, err = cfg.discover(ctx)
		if err != nil {
			return err
		}
	}
	if _, err := normalizeURL(cfg.gateway); err != nil {
		return fmt.Errorf("invalid gateway: %w", err)
	}
	if err := cfg.register(ctx); err != nil {
		log.Print("device registration failed (continuing; check API key scope): ", err)
	}
	cfg.metrics = newMetricReporter("/proc")
	go cfg.metrics.run(ctx)
	tools := newTools(cfg.home, "/system/bin/sh")
	defer tools.close()
	return connect(ctx, &cfg, user, tools)
}
