// SPDX-License-Identifier: MIT
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

	agent := &Agent{}
	agent.answerEnabled.Store(true)
	agent.messageEnabled.Store(false)

	cfg := DefaultConfig()
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
	if state["answer_calls"] != true || state["send_messages"] != false {
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
	if agent.answerEnabled.Load() || !agent.messageEnabled.Load() {
		t.Fatalf("agent flags not updated: answer=%v send=%v", agent.answerEnabled.Load(), agent.messageEnabled.Load())
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
