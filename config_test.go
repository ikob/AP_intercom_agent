// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"2s"`), &d); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if d.Duration != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d.Duration)
	}

	if err := json.Unmarshal([]byte(`3`), &d); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if d.Duration != 3*time.Second {
		t.Fatalf("expected 3s, got %v", d.Duration)
	}

	out, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `"3s"` {
		t.Fatalf("expected \"3s\", got %s", string(out))
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	data := `{
  "username": "u",
  "password": "p",
  "proxy_host": "proxy:5060",
  "register_uri": "sip:r@host",
  "message_uri": "sip:m@host",
  "entrance_uri": "sip:e@host"
}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Username != "u" || cfg.Password != "p" || cfg.ProxyHost != "proxy:5060" {
		t.Fatalf("unexpected values: %+v", cfg)
	}
	if cfg.RegisterURI == "" || cfg.MessageURI == "" || cfg.EntranceURI == "" {
		t.Fatalf("missing URIs: %+v", cfg)
	}
}

func TestSaveConfigJSON(t *testing.T) {
	out, err := SaveConfigJSON(map[string]string{"tag": "<value>"})
	if err != nil {
		t.Fatalf("SaveConfigJSON: %v", err)
	}
	if strings.Contains(string(out), "\\u003c") {
		t.Fatalf("expected unescaped <, got %s", string(out))
	}
}

func TestMissingRequiredError(t *testing.T) {
	err := missingRequiredError([]string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "missing required: a, b") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requiredArgs() []string {
	return []string{
		"-username=u",
		"-password=p",
		"-proxy=proxy:5060",
		"-register-uri=sip:r@host",
		"-message-uri=sip:m@host",
		"-entrance-uri=sip:e@host",
	}
}

func TestParseFlagsNoConfigNeeded(t *testing.T) {
	cfg, err := ParseFlags(requiredArgs())
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.LoadedConfigPath != "" {
		t.Fatalf("expected empty LoadedConfigPath, got %q", cfg.LoadedConfigPath)
	}
}

func TestParseFlagsConfigOverride(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	data := `{
  "username": "fromfile",
  "password": "p",
  "proxy_host": "proxy:5060",
  "register_uri": "sip:r@host",
  "message_uri": "sip:m@host",
  "entrance_uri": "sip:e@host",
  "answer_calls": false
}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	args := []string{
		"-config", path,
		"-username=override",
		"-allowed-caller=alice",
		"-allowed-caller=bob",
	}
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Username != "override" {
		t.Fatalf("expected username override, got %q", cfg.Username)
	}
	if cfg.LoadedConfigPath != path {
		t.Fatalf("expected LoadedConfigPath %q, got %q", path, cfg.LoadedConfigPath)
	}
	if len(cfg.AllowedCallers) != 2 || cfg.AllowedCallers[0] != "alice" || cfg.AllowedCallers[1] != "bob" {
		t.Fatalf("unexpected allowed callers: %+v", cfg.AllowedCallers)
	}
}

func TestParseFlagsMissingRequired(t *testing.T) {
	_, err := ParseFlags([]string{})
	if err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("expected missing required error, got %v", err)
	}
}

func TestParseFlagsPositionalArgs(t *testing.T) {
	args := append(requiredArgs(), "extra")
	_, err := ParseFlags(args)
	if err == nil || !strings.Contains(err.Error(), "positional args are not supported") {
		t.Fatalf("expected positional args error, got %v", err)
	}
}

func TestParseFlagsRejectStatus(t *testing.T) {
	args := append(requiredArgs(), "-reject-code=999")
	_, err := ParseFlags(args)
	if err == nil || !strings.Contains(err.Error(), "reject-code must be 480 or 503") {
		t.Fatalf("expected reject-code error, got %v", err)
	}
}

func TestStringList(t *testing.T) {
	var s StringList
	if s.String() != "" {
		t.Fatalf("expected empty string, got %q", s.String())
	}
	if err := s.Set("  a "); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set(""); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	if s.String() != "a" {
		t.Fatalf("expected a, got %q", s.String())
	}
}
