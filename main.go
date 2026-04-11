// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi
//
// Entry point.
// - Loads JSON config only when -config is provided, then overrides via flags.
// - Runs SIP agent (diago + sipgo).
// - Optionally runs an HTTP control plane for enable/disable + config retrieval.

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
)

// main initializes config and services, then runs until interrupted.
func main() {
	cfg, err := ParseFlags(os.Args[1:])
	if err != nil {
		slog.Error("failed to parse flags", "error", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	msgBuf := NewMessageBuffer(cfg.InboundMessageBuffer)

	if cfg.ContactHost == "" {
		ip, err := pickContactHost(cfg.ContactHost, cfg.BindHost, cfg.ProxyHost)
		if err != nil {
			slog.Error("failed to determine contact host", "error", err)
			os.Exit(2)
		}
		cfg.ContactHost = ip
	}
	agent, err := NewAgent(cfg, msgBuf)

	if err != nil {
		slog.Error("failed to init agent", "error", err)
		os.Exit(2)
	}
	defer agent.Close()

	// Optional HTTP control plane.
	_ = StartHTTP(ctx, cfg.HTTPListen, agent, &cfg, msgBuf)

	// Run until Ctrl+C.
	if err := agent.Run(ctx); err != nil {
		slog.Error("agent stopped with error", "error", err)
	}
}
