// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import "testing"

func TestNewAgentInvalidRegisterURI(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RegisterURI = "not-a-uri"
	cfg.MessageURI = "sip:ok@localhost"
	cfg.EntranceURI = "sip:ok@localhost"

	if _, err := NewAgent(cfg, NewMessageBuffer(1)); err == nil {
		t.Fatal("expected error for invalid register uri")
	}
}

func TestNewAgentInvalidMessageURI(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RegisterURI = "sip:ok@localhost"
	cfg.MessageURI = "bad"
	cfg.EntranceURI = "sip:ok@localhost"

	if _, err := NewAgent(cfg, NewMessageBuffer(1)); err == nil {
		t.Fatal("expected error for invalid message uri")
	}
}

func TestNewAgentInvalidEntranceURI(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RegisterURI = "sip:ok@localhost"
	cfg.MessageURI = "sip:ok@localhost"
	cfg.EntranceURI = "bad"

	if _, err := NewAgent(cfg, NewMessageBuffer(1)); err == nil {
		t.Fatal("expected error for invalid entrance uri")
	}
}
