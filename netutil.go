// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
)

// pickContactHost picks the Contact host using explicit value, bind host, or proxy-route inference.
func pickContactHost(contactHost, bindHost, proxyHost string) (string, error) {
	if contactHost != "" {
		return contactHost, nil
	}

	// If bindHost is a concrete IP, it's usually the best guess.
	if bindHost != "" && bindHost != "0.0.0.0" && bindHost != "::" {
		return bindHost, nil
	}

	host, port, err := net.SplitHostPort(proxyHost)
	if err != nil {
		// If proxyHost is not host:port, we cannot infer reliably.
		return "", fmt.Errorf("proxyHost must be host:port for auto contact-host: %w", err)
	}
	if port == "" {
		return "", fmt.Errorf("proxyHost port is empty")
	}

	// Dial UDP to let the OS pick the outgoing interface/address.
	conn, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return "", fmt.Errorf("udp dial to proxy failed: %w", err)
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return "", fmt.Errorf("failed to determine local UDP address")
	}

	ip := localAddr.IP.String()
	// Normalize IPv6 zone if any (e.g., "fe80::1%en0").
	ip = strings.Split(ip, "%")[0]
	slog.Info("Inferred contact host", "contact_host", ip, "proxy_host", proxyHost)
	return ip, nil
}
