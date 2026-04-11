// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

func TestContactHeaderValue(t *testing.T) {
	a := &Agent{
		cfg: Config{
			Username:      "user",
			ContactHost:   "127.0.0.1",
			ContactPort:   5060,
			ContactParams: ";foo=bar",
		},
	}

	got := a.contactHeaderValue()
	want := "<sip:user@127.0.0.1:5060>;foo=bar"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestSetEnabled(t *testing.T) {
	var b atomic.Bool
	b.Store(false)

	a := &Agent{}
	a.SetEnabled(&b, true)
	if !b.Load() {
		t.Fatal("expected enabled to be true")
	}
}

func TestSetAnswerMessageFlagsEnabled(t *testing.T) {
	a := &Agent{}
	a.answerEnabled.Store(false)
	a.messageEnabled.Store(false)
	a.answerMessageEnabled.Store(false)

	a.SetAnswerEnabled(true)
	a.SetMessageEnabled(true)
	a.SetAnswerMessageEnabled(true)

	if !a.answerEnabled.Load() || !a.messageEnabled.Load() || !a.answerMessageEnabled.Load() {
		t.Fatalf("expected all flags true: answer=%v message=%v answer_message=%v", a.answerEnabled.Load(), a.messageEnabled.Load(), a.answerMessageEnabled.Load())
	}
}

func TestRegOpts(t *testing.T) {
	a := &Agent{
		cfg: Config{
			Username:      "user",
			Password:      "pass",
			ProxyHost:     "proxy:5060",
			Expiry:        Duration{Duration: 10 * time.Second},
			RetryInterval: Duration{Duration: 2 * time.Second},
		},
	}

	opts := a.regOpts()
	if opts.Username != "user" || opts.Password != "pass" || opts.ProxyHost != "proxy:5060" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if opts.Expiry != 10*time.Second || opts.RetryInterval != 2*time.Second {
		t.Fatalf("unexpected timings: %+v", opts)
	}
}

func TestParseExpiryValue(t *testing.T) {
	if v, ok := parseExpiryValue("120"); !ok || v != 120 {
		t.Fatalf("expected 120 true, got %d %v", v, ok)
	}
	if _, ok := parseExpiryValue("bad"); ok {
		t.Fatal("expected invalid parse to return false")
	}
	if _, ok := parseExpiryValue("0"); ok {
		t.Fatal("expected zero to return false")
	}
}

func TestParseExpiryParam(t *testing.T) {
	if _, ok := parseExpiryParam("", false); ok {
		t.Fatal("expected missing param to return false")
	}
	if v, ok := parseExpiryParam("60", true); !ok || v != 60 {
		t.Fatalf("expected 60 true, got %d %v", v, ok)
	}
}

func TestSetupLogger(t *testing.T) {
	t.Setenv("RTP_DEBUG", "true")
	t.Setenv("RTCP_DEBUG", "true")
	t.Setenv("SIP_DEBUG", "true")
	t.Setenv("SIP_TRANSACTION_DEBUG", "true")

	setupLogger("debug")

	if !media.RTPDebug || !media.RTCPDebug {
		t.Fatalf("expected RTP/RTCP debug to be enabled: rtp=%v rtcp=%v", media.RTPDebug, media.RTCPDebug)
	}
	if !sip.SIPDebug || !sip.TransactionFSMDebug {
		t.Fatalf("expected SIP debug to be enabled: sip=%v tx=%v", sip.SIPDebug, sip.TransactionFSMDebug)
	}
}

func TestSleepOrDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepOrDone(ctx, 50*time.Millisecond) {
		t.Fatal("expected sleepOrDone to return false when canceled")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if !sleepOrDone(ctx2, 50*time.Millisecond) {
		t.Fatal("expected sleepOrDone to return true after timer")
	}
}

func TestRegisterLoopStartStop(t *testing.T) {
	a := &Agent{}
	a.registerLoopFn = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a.startRegisterLoop(ctx)
	if a.regID != 1 {
		t.Fatalf("expected regID 1, got %d", a.regID)
	}

	a.startRegisterLoop(ctx)
	if a.regID != 1 {
		t.Fatalf("expected regID to remain 1, got %d", a.regID)
	}

	a.stopRegisterLoop()
	if a.regCancel != nil {
		t.Fatal("expected regCancel to be nil after stop")
	}
	select {
	case <-a.regDone:
	default:
		t.Fatal("expected regDone to be closed")
	}
}

type fakeDialog struct {
	calls []string
}

func (f *fakeDialog) Trying() error {
	f.calls = append(f.calls, "trying")
	return nil
}

func (f *fakeDialog) Ringing() error {
	f.calls = append(f.calls, "ringing")
	return nil
}

func (f *fakeDialog) Answer() error {
	f.calls = append(f.calls, "answer")
	return nil
}

func (f *fakeDialog) Hangup(ctx context.Context) error {
	f.calls = append(f.calls, "hangup")
	return nil
}

func TestRespondIncomingCallOrder(t *testing.T) {
	a := &Agent{}
	d := &fakeDialog{}

	if err := a.respondIncomingCallWith(context.Background(), "test", d); err != nil {
		t.Fatalf("respondIncomingCallWith failed: %v", err)
	}

	want := []string{"trying", "ringing", "answer", "hangup"}
	if len(d.calls) != len(want) {
		t.Fatalf("unexpected calls: %v", d.calls)
	}
	for i, v := range want {
		if d.calls[i] != v {
			t.Fatalf("expected %q at %d, got %q", v, i, d.calls[i])
		}
	}
}
