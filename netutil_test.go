// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"strings"
	"testing"
)

func TestPickContactHostUsesContactHost(t *testing.T) {
	host, err := pickContactHost("1.2.3.4", "", "bad")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "1.2.3.4" {
		t.Fatalf("expected contact host, got %q", host)
	}
}

func TestPickContactHostUsesBindHost(t *testing.T) {
	host, err := pickContactHost("", "10.0.0.5", "bad")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "10.0.0.5" {
		t.Fatalf("expected bind host, got %q", host)
	}
}

func TestPickContactHostProxyHostInvalid(t *testing.T) {
	_, err := pickContactHost("", "0.0.0.0", "bad")
	if err == nil || !strings.Contains(err.Error(), "proxyHost must be host:port") {
		t.Fatalf("expected host:port error, got %v", err)
	}
}

func TestPickContactHostProxyHostEmptyPort(t *testing.T) {
	_, err := pickContactHost("", "0.0.0.0", "127.0.0.1:")
	if err == nil || !strings.Contains(err.Error(), "proxyHost port is empty") {
		t.Fatalf("expected empty port error, got %v", err)
	}
}

func TestPickContactHostDialProxy(t *testing.T) {
	host, err := pickContactHost("", "0.0.0.0", "127.0.0.1:9")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("udp dial not permitted: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if host == "" {
		t.Fatal("expected non-empty host")
	}
}
