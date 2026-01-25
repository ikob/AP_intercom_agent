// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestSendMessageDigestAuth(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(_ context.Context, req *sip.Request) (*sip.Response, error) {
			return sip.NewResponseFromRequest(req, 407, "Proxy Authentication Required", nil), nil
		},
		digestFn: func(_ context.Context, req *sip.Request, _ *sip.Response, _ sipgo.DigestAuth) (*sip.Response, error) {
			return sip.NewResponseFromRequest(req, 200, "OK", nil), nil
		},
	}
	agent.cli = fake

	target := sip.Uri{User: "bob", Host: "127.0.0.1"}
	body := []byte("hello")
	if err := agent.sendMessage(context.Background(), target, "text/plain", body); err != nil {
		t.Fatalf("sendMessage failed: %v", err)
	}

	if len(fake.doCalls) != 1 {
		t.Fatalf("expected 1 Do call, got %d", len(fake.doCalls))
	}
	if len(fake.digestCalls) != 1 {
		t.Fatalf("expected 1 DoDigestAuth call, got %d", len(fake.digestCalls))
	}

	req := fake.doCalls[0]
	if h := req.GetHeader("Content-Type"); h == nil || h.Value() != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %v", h)
	}
	if h := req.GetHeader("Contact"); h == nil || h.Value() != agent.contactHeaderValue() {
		t.Fatalf("expected Contact %q, got %v", agent.contactHeaderValue(), h)
	}
	if got := string(req.Body()); got != "hello" {
		t.Fatalf("expected body hello, got %q", got)
	}
}

func TestSendMessageNon2xx(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(_ context.Context, req *sip.Request) (*sip.Response, error) {
			return sip.NewResponseFromRequest(req, 500, "Server Error", nil), nil
		},
	}
	agent.cli = fake

	target := sip.Uri{User: "bob", Host: "127.0.0.1"}
	if err := agent.sendMessage(context.Background(), target, "text/plain", []byte("hello")); err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestSendMessageDigestNon2xx(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(_ context.Context, req *sip.Request) (*sip.Response, error) {
			return sip.NewResponseFromRequest(req, 401, "Unauthorized", nil), nil
		},
		digestFn: func(_ context.Context, req *sip.Request, _ *sip.Response, _ sipgo.DigestAuth) (*sip.Response, error) {
			return sip.NewResponseFromRequest(req, 403, "Forbidden", nil), nil
		},
	}
	agent.cli = fake

	target := sip.Uri{User: "bob", Host: "127.0.0.1"}
	if err := agent.sendMessage(context.Background(), target, "text/plain", []byte("hello")); err == nil {
		t.Fatal("expected error for non-2xx digest response")
	}
}

func TestSendMessageDoError(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(_ context.Context, _ *sip.Request) (*sip.Response, error) {
			return nil, errors.New("network error")
		},
	}
	agent.cli = fake

	target := sip.Uri{User: "bob", Host: "127.0.0.1"}
	if err := agent.sendMessage(context.Background(), target, "text/plain", []byte("hello")); err == nil {
		t.Fatal("expected error for client Do failure")
	}
}

func TestInboundMessageBuffered(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)

	agent, err := NewAgent(cfg, NewMessageBuffer(2))
	if err != nil {
		t.Fatal(err)
	}

	agent.registerLoopFn = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = agent.Run(ctx)
	}()
	t.Cleanup(agent.Close)

	client, clientAddr, cleanup := startUAC(t)
	defer cleanup()

	req := sip.NewRequest(sip.MESSAGE, sip.Uri{User: "agent", Host: "127.0.0.1", Port: port})
	req.AppendHeader(sip.NewHeader("Contact", "<sip:alice@"+clientAddr+">"))
	req.AppendHeader(sip.NewHeader("Content-Type", "text/plain"))
	req.SetBody([]byte("hello"))

	res, err := client.Do(context.Background(), req, sipgo.ClientRequestBuild)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs := agent.msgBuf.Snapshot()
		if len(msgs) > 0 {
			got := msgs[len(msgs)-1]
			if got.ContentType != "text/plain" {
				t.Fatalf("expected content-type text/plain, got %q", got.ContentType)
			}
			if string(got.Body) != "hello" {
				t.Fatalf("expected body hello, got %q", string(got.Body))
			}
			if got.CallID == "" {
				t.Fatal("expected call-id to be set")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for inbound message")
}
