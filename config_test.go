// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestDefaultConfigUsesSafeRuntimeControls(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.AnswerCalls || cfg.SendMessages || !cfg.CaptureImages {
		t.Fatalf("defaults answer=%v send=%v capture=%v, want false/false/true", cfg.AnswerCalls, cfg.SendMessages, cfg.CaptureImages)
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
	if cfg.JPEGQueryS != defaultJPEGQueryS {
		t.Fatalf("JPEG query s = %q, want default %q", cfg.JPEGQueryS, defaultJPEGQueryS)
	}
	if cfg.IncomingJPEGMaxFiles != 100 {
		t.Fatalf("incoming JPEG max files = %d, want 100", cfg.IncomingJPEGMaxFiles)
	}
	if !cfg.CaptureImages {
		t.Fatal("capture_images should use the safe image-agent default when both toggles are omitted")
	}
	if cfg.SendMessages || cfg.AnswerCalls {
		t.Fatalf("safe defaults answer_calls=%v send_messages=%v, want false/false", cfg.AnswerCalls, cfg.SendMessages)
	}
	if len(cfg.UnlockCallers) != 1 || cfg.UnlockCallers[0] != "interphone0" {
		t.Fatalf("unlock callers = %v, want [interphone0]", cfg.UnlockCallers)
	}
}

func TestLoadConfigFileCaptureImagesCompatibility(t *testing.T) {
	tests := []struct {
		name             string
		body             string
		wantCapture      bool
		wantExplicitFlag bool
	}{
		{
			name:             "new config uses independent capture default",
			body:             `{}`,
			wantCapture:      true,
			wantExplicitFlag: true,
		},
		{
			name:             "legacy config follows disabled messages",
			body:             `{"send_messages": false}`,
			wantCapture:      false,
			wantExplicitFlag: false,
		},
		{
			name:             "legacy config follows enabled messages",
			body:             `{"send_messages": true}`,
			wantCapture:      true,
			wantExplicitFlag: false,
		},
		{
			name:             "explicit capture is independent",
			body:             `{"send_messages": false, "capture_images": true}`,
			wantCapture:      true,
			wantExplicitFlag: true,
		},
		{
			name:             "explicit disabled capture is independent",
			body:             `{"send_messages": true, "capture_images": false}`,
			wantCapture:      false,
			wantExplicitFlag: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CaptureImages != test.wantCapture || cfg.captureImagesSet != test.wantExplicitFlag {
				t.Fatalf("capture_images=%v explicit=%v, want %v/%v", cfg.CaptureImages, cfg.captureImagesSet, test.wantCapture, test.wantExplicitFlag)
			}
		})
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
	if cfg.AnswerCalls || cfg.SendMessages || !cfg.CaptureImages {
		t.Fatalf("safe defaults answer=%v send=%v capture=%v, want false/false/true", cfg.AnswerCalls, cfg.SendMessages, cfg.CaptureImages)
	}
}

func TestParseFlagsCaptureImagesCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCapture bool
	}{
		{
			name:        "automatic unlock flag does not disable default capture",
			args:        []string{"-send-messages=false"},
			wantCapture: true,
		},
		{
			name:        "explicit capture overrides disabled messages",
			args:        []string{"-send-messages=false", "-capture-images=true"},
			wantCapture: true,
		},
		{
			name:        "explicit capture overrides enabled messages",
			args:        []string{"-send-messages=true", "-capture-images=false"},
			wantCapture: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := ParseFlags(append(requiredArgs(), test.args...))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CaptureImages != test.wantCapture {
				t.Fatalf("capture_images=%v, want %v", cfg.CaptureImages, test.wantCapture)
			}
		})
	}
}

func TestParseFlagsPreservesExplicitConfigCaptureImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
  "username": "u",
  "password": "p",
  "proxy_host": "proxy:5060",
  "register_uri": "sip:r@host",
  "message_uri": "sip:m@host",
  "entrance_uri": "sip:e@host",
  "send_messages": false,
  "capture_images": false
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseFlags([]string{"-config=" + path, "-send-messages=true"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SendMessages || cfg.CaptureImages {
		t.Fatalf("send_messages=%v capture_images=%v, want true/false", cfg.SendMessages, cfg.CaptureImages)
	}
}

func TestParseFlagsPasswordFlagOverridesEnvironment(t *testing.T) {
	t.Setenv(passwordEnvVar, "from-environment")
	cfg, err := ParseFlags(requiredArgs())
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Password != "p" {
		t.Fatalf("password = %q, want command-line value", cfg.Password)
	}
}

func TestParseFlagsPasswordFromEnvironment(t *testing.T) {
	t.Setenv(passwordEnvVar, "from-environment")
	args := []string{
		"-username=u",
		"-proxy=proxy:5060",
		"-register-uri=sip:r@host",
		"-message-uri=sip:m@host",
		"-entrance-uri=sip:e@host",
	}

	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Password != "from-environment" {
		t.Fatalf("password = %q, want value from %s", cfg.Password, passwordEnvVar)
	}
}

func TestParseFlagsConfigOverride(t *testing.T) {
	t.Setenv(passwordEnvVar, "from-environment")
	dir := t.TempDir()
	path := dir + "/config.json"

	data := `{
  "username": "fromfile",
  "password": "p",
  "proxy_host": "proxy:5060",
  "register_uri": "sip:r@host",
  "message_uri": "sip:m@host",
  "entrance_uri": "sip:e@host",
	"jpeg_query_s": "custom-s",
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
	if cfg.Password != "p" {
		t.Fatalf("password = %q, want JSON configuration value", cfg.Password)
	}
	if cfg.JPEGQueryS != "custom-s" {
		t.Fatalf("expected JPEG query override, got %q", cfg.JPEGQueryS)
	}
	if cfg.LoadedConfigPath != path {
		t.Fatalf("expected LoadedConfigPath %q, got %q", path, cfg.LoadedConfigPath)
	}
	if len(cfg.AllowedCallers) != 2 || cfg.AllowedCallers[0] != "alice" || cfg.AllowedCallers[1] != "bob" {
		t.Fatalf("unexpected allowed callers: %+v", cfg.AllowedCallers)
	}
}

func TestParseFlagsMissingRequired(t *testing.T) {
	t.Setenv(passwordEnvVar, "")
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
	if err == nil || !strings.Contains(err.Error(), "reject-code must be 480, 486, or 503") {
		t.Fatalf("expected reject-code error, got %v", err)
	}
}

func TestParseFlagsAllowsBusyHere(t *testing.T) {
	args := append(requiredArgs(), "-reject-code=486", "-reject-reason=Busy Here")
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RejectStatus != 486 || cfg.RejectReason != "Busy Here" {
		t.Fatalf("unexpected reject config: %+v", cfg)
	}
}

func TestParseFlagsMonitorProbe(t *testing.T) {
	jpegOut := filepath.Join(t.TempDir(), "frame.jpg")
	args := append(requiredArgs(),
		"-monitor-probe",
		"-monitor-uri=sip:monitor@host",
		"-monitor-hold=250ms",
		"-monitor-repeat=2",
		"-monitor-jpeg-out="+jpegOut,
		"-monitor-jpeg-interval=50ms",
	)
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cfg.MonitorProbe || cfg.MonitorURI != "sip:monitor@host" {
		t.Fatalf("unexpected monitor config: %+v", cfg)
	}
	if cfg.MonitorHold.Duration != 250*time.Millisecond || cfg.MonitorRepeat != 2 ||
		cfg.MonitorJPEGOut != jpegOut || cfg.MonitorJPEGInterval.Duration != 50*time.Millisecond {
		t.Fatalf("unexpected monitor timings: %+v", cfg)
	}
}

func TestParseFlagsMonitorProbeRequiresURI(t *testing.T) {
	args := append(requiredArgs(), "-monitor-probe")
	_, err := ParseFlags(args)
	if err == nil || !strings.Contains(err.Error(), "monitor-uri") {
		t.Fatalf("expected monitor-uri error, got %v", err)
	}
}

func TestParseFlagsMonitorJPEGValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "requires probe",
			args:    []string{"-monitor-jpeg-out=/tmp/frame.jpg"},
			wantErr: "requires monitor-probe",
		},
		{
			name: "absolute output",
			args: []string{
				"-monitor-probe", "-monitor-uri=sip:monitor@host",
				"-monitor-jpeg-out=frame.jpg",
			},
			wantErr: "absolute path",
		},
		{
			name: "positive hold",
			args: []string{
				"-monitor-probe", "-monitor-uri=sip:monitor@host",
				"-monitor-jpeg-out=/tmp/frame.jpg", "-monitor-hold=0",
			},
			wantErr: "monitor-hold must be positive",
		},
		{
			name: "positive interval",
			args: []string{
				"-monitor-probe", "-monitor-uri=sip:monitor@host",
				"-monitor-jpeg-out=/tmp/frame.jpg", "-monitor-jpeg-interval=0",
			},
			wantErr: "monitor-jpeg-interval must be positive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(requiredArgs(), test.args...)
			_, err := ParseFlags(args)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestParseFlagsIncomingJPEG(t *testing.T) {
	directory := t.TempDir()
	args := append(requiredArgs(),
		"-incoming-jpeg-dir="+directory,
		"-incoming-jpeg-hold=750ms",
		"-incoming-jpeg-interval=125ms",
		"-incoming-jpeg-max-files=42",
		"-jpeg-query-s=custom/value+1",
	)
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IncomingJPEGDir != directory || cfg.IncomingJPEGHold.Duration != 750*time.Millisecond ||
		cfg.IncomingJPEGInterval.Duration != 125*time.Millisecond || cfg.IncomingJPEGMaxFiles != 42 ||
		cfg.JPEGQueryS != "custom/value+1" {
		t.Fatalf("unexpected incoming JPEG config: %+v", cfg)
	}
}

func TestLoadConfigOverridesUnlockCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
  "unlock_callers": ["door-a", "door-b"]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"door-a", "door-b"}
	if len(cfg.UnlockCallers) != len(want) || cfg.UnlockCallers[0] != want[0] || cfg.UnlockCallers[1] != want[1] {
		t.Fatalf("unlock callers = %v, want %v", cfg.UnlockCallers, want)
	}
}

func TestParseFlagsIncomingJPEGValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "absolute directory", args: []string{"-incoming-jpeg-dir=frames"}, wantErr: "absolute path"},
		{name: "positive hold", args: []string{"-incoming-jpeg-dir=/tmp/frames", "-incoming-jpeg-hold=0"}, wantErr: "incoming-jpeg-hold must be positive"},
		{name: "positive interval", args: []string{"-incoming-jpeg-dir=/tmp/frames", "-incoming-jpeg-interval=0"}, wantErr: "incoming-jpeg-interval must be positive"},
		{name: "positive max files", args: []string{"-incoming-jpeg-dir=/tmp/frames", "-incoming-jpeg-max-files=0"}, wantErr: "incoming-jpeg-max-files must be at least 1"},
		{name: "nonempty query s", args: []string{"-jpeg-query-s="}, wantErr: "jpeg-query-s must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(requiredArgs(), test.args...)
			_, err := ParseFlags(args)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestParseFlagsUnlockCallersReplaceDefault(t *testing.T) {
	args := append(requiredArgs(), "-unlock-caller=door-a", "-unlock-caller=door-b")
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"door-a", "door-b"}
	if len(cfg.UnlockCallers) != len(want) || cfg.UnlockCallers[0] != want[0] || cfg.UnlockCallers[1] != want[1] {
		t.Fatalf("unlock callers = %v, want %v", cfg.UnlockCallers, want)
	}
}

func TestParseFlagsUnlockCallersCanDisableAll(t *testing.T) {
	args := append(requiredArgs(), "-unlock-caller=")
	cfg, err := ParseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.UnlockCallers) != 0 {
		t.Fatalf("unlock callers = %v, want none", cfg.UnlockCallers)
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
