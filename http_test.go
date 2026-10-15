// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func startHTTPTestServer(t *testing.T) (string, context.CancelFunc, *Agent, *Config, *MessageBuffer) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	addr := freeTCPAddr(t)

	agent := &Agent{frameHub: NewFrameHub(), audioHub: NewAudioHub()}
	agent.answerEnabled.Store(true)
	agent.messageEnabled.Store(false)
	agent.captureEnabled.Store(true)

	cfg := DefaultConfig()
	cfg.Password = "http-test-secret"
	cfg.RingWebhookURL = "http://127.0.0.1:8123/api/webhook/http-test-secret"
	cfg.LoadedConfigPath = "/tmp/config.json"
	msg := NewMessageBuffer(2)
	msg.Add(InboundMessage{CallID: "id-1", Body: []byte("hello")})

	srv := StartHTTP(ctx, addr, agent, &cfg, msg)
	if srv == nil {
		cancel()
		t.Fatal("StartHTTP returned nil")
	}

	baseURL := "http://" + addr
	client := http.Client{Timeout: 200 * time.Millisecond}
	ready := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = res.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatal("server did not become ready")
	}

	return baseURL, cancel, agent, &cfg, msg
}

func TestStartHTTPDisabled(t *testing.T) {
	if srv := StartHTTP(context.Background(), "", &Agent{}, &Config{}, NewMessageBuffer(1)); srv != nil {
		t.Fatal("expected nil server when listen is empty")
	}
}

func TestHTTPHealthzAndConfig(t *testing.T) {
	baseURL, cancel, _, cfg, _ := startHTTPTestServer(t)
	defer cancel()

	client := http.Client{Timeout: 500 * time.Millisecond}

	res, err := client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz status: %d", res.StatusCode)
	}

	res, err = client.Get(baseURL + "/v1/config")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("config status: %d", res.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if payload["config_path"] != cfg.LoadedConfigPath {
		t.Fatalf("unexpected config_path: %v", payload["config_path"])
	}
	runtime, ok := payload["runtime"].(map[string]any)
	if !ok || runtime["capture_images"] != true {
		t.Fatalf("runtime does not expose capture_images: %#v", payload["runtime"])
	}
	config, ok := payload["config"].(map[string]any)
	if !ok || config["capture_images"] != true {
		t.Fatalf("config does not expose capture_images: %#v", payload["config"])
	}
	if config["password"] != "" || payload["password_configured"] != true {
		t.Fatalf("config password was not safely redacted: %#v", payload)
	}
	if value, present := config["ring_webhook_url"]; (present && value != "") || payload["ring_webhook_configured"] != true {
		t.Fatalf("ring webhook URL was not safely redacted: %#v", payload)
	}
}

func TestHTTPStateAndMessages(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()

	client := http.Client{Timeout: 500 * time.Millisecond}

	res, err := client.Get(baseURL + "/v1/state")
	if err != nil {
		t.Fatalf("state get: %v", err)
	}
	defer res.Body.Close()

	var state map[string]any
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state["answer_calls"] != true || state["send_messages"] != false || state["capture_images"] != true {
		t.Fatalf("unexpected state: %#v", state)
	}

	body := []byte(`{"answer_calls": false, "send_messages": true}`)
	res, err = client.Post(baseURL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("state post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("state post status: %d", res.StatusCode)
	}
	if agent.answerEnabled.Load() || !agent.messageEnabled.Load() || !agent.captureEnabled.Load() {
		t.Fatalf("agent flags not updated or unspecified capture changed: answer=%v send=%v capture=%v", agent.answerEnabled.Load(), agent.messageEnabled.Load(), agent.captureEnabled.Load())
	}

	body = []byte(`{"capture_images": false}`)
	res, err = client.Post(baseURL+"/v1/state", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("capture state post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("capture state post status: %d", res.StatusCode)
	}
	if agent.answerEnabled.Load() || !agent.messageEnabled.Load() || agent.captureEnabled.Load() {
		t.Fatalf("partial capture update changed another flag: answer=%v send=%v capture=%v", agent.answerEnabled.Load(), agent.messageEnabled.Load(), agent.captureEnabled.Load())
	}

	res, err = client.Get(baseURL + "/v1/messages")
	if err != nil {
		t.Fatalf("messages get: %v", err)
	}
	defer res.Body.Close()

	var msgs []InboundMessage
	if err := json.NewDecoder(res.Body).Decode(&msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].CallID != "id-1" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestHTTPCallStateAndActionsRequireCurrentSession(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()

	session := &incomingCallSession{
		sessionID: "session-current",
		commands:  make(chan incomingCallCommand, 2),
	}
	agent.callMu.Lock()
	agent.activeCall = session
	agent.callSnapshot = CallSnapshot{
		State:     CallStateRinging,
		SessionID: session.sessionID,
		Caller:    "interphone0",
	}
	agent.callMu.Unlock()

	response, err := http.Get(baseURL + "/v1/call")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot CallSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != CallStateRinging || snapshot.SessionID != session.sessionID {
		t.Fatalf("unexpected call snapshot: %+v", snapshot)
	}

	response, err = http.Post(baseURL+"/v1/call/answer", "application/json", bytes.NewBufferString(`{"session_id":"stale"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale answer status = %d, want 409", response.StatusCode)
	}

	response, err = http.Post(baseURL+"/v1/call/answer", "application/json", bytes.NewBufferString(`{"session_id":"session-current"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("answer status = %d, want 202", response.StatusCode)
	}
	select {
	case command := <-session.commands:
		if command != incomingCallAnswer {
			t.Fatalf("command = %v, want answer", command)
		}
	case <-time.After(time.Second):
		t.Fatal("answer command was not queued")
	}

	agent.callMu.Lock()
	agent.callSnapshot.State = CallStateConnected
	agent.callMu.Unlock()
	response, err = http.Post(baseURL+"/v1/call/hangup", "application/json", bytes.NewBufferString(`{"session_id":"session-current"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("hangup status = %d, want 202", response.StatusCode)
	}
	select {
	case command := <-session.commands:
		if command != incomingCallHangup {
			t.Fatalf("command = %v, want hangup", command)
		}
	case <-time.After(time.Second):
		t.Fatal("hangup command was not queued")
	}
}

func TestHTTPCallIdleSnapshotOmitsActiveMetadata(t *testing.T) {
	baseURL, cancel, _, _, _ := startHTTPTestServer(t)
	defer cancel()
	response, err := http.Get(baseURL + "/v1/call")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["state"] != string(CallStateIdle) || payload["monitor_active"] != false {
		t.Fatalf("unexpected idle snapshot: %#v", payload)
	}
	for _, key := range []string{"session_id", "caller", "call_id", "dialog_id", "started_at"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("idle snapshot unexpectedly contains %s: %#v", key, payload)
		}
	}
}
