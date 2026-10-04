// SPDX-License-Identifier: Apache-2.0
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

type digestCall struct {
	req  *sip.Request
	res  *sip.Response
	auth sipgo.DigestAuth
}

type fakeClient struct {
	doFn        func(ctx context.Context, req *sip.Request) (*sip.Response, error)
	digestFn    func(ctx context.Context, req *sip.Request, res *sip.Response, auth sipgo.DigestAuth) (*sip.Response, error)
	doCalls     []*sip.Request
	digestCalls []digestCall
}

func (f *fakeClient) Do(ctx context.Context, req *sip.Request, _ ...sipgo.ClientRequestOption) (*sip.Response, error) {
	f.doCalls = append(f.doCalls, req)
	if f.doFn == nil {
		return nil, errors.New("fake client Do not configured")
	}
	return f.doFn(ctx, req)
}

func (f *fakeClient) DoDigestAuth(ctx context.Context, req *sip.Request, res *sip.Response, auth sipgo.DigestAuth) (*sip.Response, error) {
	f.digestCalls = append(f.digestCalls, digestCall{req: req, res: res, auth: auth})
	if f.digestFn == nil {
		return nil, errors.New("fake client DoDigestAuth not configured")
	}
	return f.digestFn(ctx, req, res, auth)
}

func (f *fakeClient) Close() error {
	return nil
}

func testConfig(port int) Config {
	cfg := DefaultConfig()
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = port
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = port
	cfg.ContactParams = ""
	cfg.Username = "alice"
	cfg.Password = "secret"
	cfg.ProxyHost = "127.0.0.1:5060"
	cfg.SendMessages = false
	cfg.RegisterURI = "sip:agent@127.0.0.1:5060"
	cfg.MessageURI = "sip:agent@127.0.0.1:5060"
	cfg.EntranceURI = "sip:agent@127.0.0.1:5060"
	return cfg
}

func TestRegisterDigestAuth(t *testing.T) {
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
			return sip.NewResponseFromRequest(req, 200, "OK", nil), nil
		},
	}
	agent.cli = fake

	if _, err := agent.doRegisterOnce(context.Background(), 60); err != nil {
		t.Fatalf("doRegisterOnce failed: %v", err)
	}

	if len(fake.doCalls) != 1 {
		t.Fatalf("expected 1 Do call, got %d", len(fake.doCalls))
	}
	if len(fake.digestCalls) != 1 {
		t.Fatalf("expected 1 DoDigestAuth call, got %d", len(fake.digestCalls))
	}

	req := fake.doCalls[0]
	if h := req.GetHeader("Expires"); h == nil || h.Value() != "60" {
		t.Fatalf("expected Expires=60, got %v", h)
	}
	if h := req.GetHeader("Contact"); h == nil || h.Value() != agent.contactHeaderValue() {
		t.Fatalf("expected Contact %q, got %v", agent.contactHeaderValue(), h)
	}

	gotAuth := fake.digestCalls[0].auth
	if gotAuth.Username != cfg.Username || gotAuth.Password != cfg.Password {
		t.Fatalf("unexpected auth: %+v", gotAuth)
	}
}

func TestRegisterNon2xx(t *testing.T) {
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

	if _, err := agent.doRegisterOnce(context.Background(), 60); err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestRegisterDigestNon2xx(t *testing.T) {
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

	if _, err := agent.doRegisterOnce(context.Background(), 60); err == nil {
		t.Fatal("expected error for non-2xx digest response")
	}
}

func TestRegisterDoError(t *testing.T) {
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

	if _, err := agent.doRegisterOnce(context.Background(), 60); err == nil {
		t.Fatal("expected error for client Do failure")
	}
}

func TestRegisterLoopContextCanceled(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)
	cfg.RetryInterval.Duration = 10 * time.Millisecond

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
			return nil, ctx.Err()
		},
	}
	agent.cli = fake

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := agent.registerLoop(ctx); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRegisterLoopRetryCanceled(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)
	cfg.RetryInterval.Duration = 10 * time.Millisecond

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := agent.registerLoop(ctx); err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestResponseExpirySeconds(t *testing.T) {
	req := sip.NewRequest(sip.REGISTER, sip.Uri{User: "agent", Host: "localhost"})
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Expires", "120"))

	secs, ok := responseExpirySeconds(res)
	if !ok || secs != 120 {
		t.Fatalf("expected 120 true, got %d %v", secs, ok)
	}

	res = sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Expires", "bad"))
	if _, ok := responseExpirySeconds(res); ok {
		t.Fatal("expected invalid Expires to be rejected")
	}
}

func TestResponseExpirySecondsFromContactParam(t *testing.T) {
	req := sip.NewRequest(sip.REGISTER, sip.Uri{User: "agent", Host: "localhost"})
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Contact", "<sip:agent@localhost>;expires=1200"))

	secs, ok := responseExpirySeconds(res)
	if !ok || secs != 1200 {
		t.Fatalf("expected 1200 true, got %d %v", secs, ok)
	}
}

func TestResponseExpirySecondsUsesShorterValue(t *testing.T) {
	req := sip.NewRequest(sip.REGISTER, sip.Uri{User: "agent", Host: "localhost"})
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Expires", "3600"))
	res.AppendHeader(sip.NewHeader("Contact", "<sip:agent@localhost>;expires=1200"))

	secs, ok := responseExpirySeconds(res)
	if !ok || secs != 1200 {
		t.Fatalf("expected 1200 true, got %d %v", secs, ok)
	}
}

func TestRegisterLoopWaitUsesResponseExpiry(t *testing.T) {
	port := freeUDPPort(t)
	cfg := testConfig(port)
	cfg.Expiry.Duration = 30 * time.Second
	cfg.RetryInterval.Duration = 10 * time.Second

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeClient{
		doFn: func(_ context.Context, req *sip.Request) (*sip.Response, error) {
			res := sip.NewResponseFromRequest(req, 200, "OK", nil)
			res.AppendHeader(sip.NewHeader("Expires", "12"))
			return res, nil
		},
	}
	agent.cli = fake

	waitCh := make(chan time.Duration, 1)
	ctx, cancel := context.WithCancel(context.Background())
	agent.sleepFn = func(_ context.Context, d time.Duration) bool {
		waitCh <- d
		cancel()
		return false
	}

	_ = agent.registerLoop(ctx)

	wait := <-waitCh
	if wait < 7*time.Second || wait > 9*time.Second {
		t.Fatalf("expected wait about 8s, got %v", wait)
	}
}
