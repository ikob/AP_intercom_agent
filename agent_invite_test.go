// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

// agent_invite_test.go
package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestInviteAnswered(t *testing.T) {
	port := freeUDPPort(t)

	cfg := DefaultConfig()
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = port
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = port
	cfg.ContactParams = ""
	cfg.AnswerCalls = true
	cfg.SendMessages = false
	cfg.AllowedCallers = nil
	cfg.RegisterURI = "sip:agent@127.0.0.1:5060"
	cfg.MessageURI = "sip:agent@127.0.0.1:5060"
	cfg.EntranceURI = "sip:agent@127.0.0.1:5060"

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}

	// Do not register during tests.
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

	byeCh := make(chan *sip.Request, 1)
	client, clientAddr, cleanup := startUACWithBye(t, byeCh)
	defer cleanup()

	// Minimal SDP.
	body := sdp.GenerateForAudio(
		net.IPv4(127, 0, 0, 1),
		net.IPv4(127, 0, 0, 1),
		40000,
		sdp.ModeSendrecv,
		[]string{sdp.FORMAT_TYPE_ALAW},
	)

	host, portNum, err := sip.ParseAddr(clientAddr)
	if err != nil {
		t.Fatalf("parse client addr: %v", err)
	}
	dialogUA := &sipgo.DialogUA{
		Client: client,
		ContactHDR: sip.ContactHeader{
			Address: sip.Uri{
				User: "alice",
				Host: host,
				Port: portNum,
			},
		},
	}

	dlg, err := dialogUA.Invite(context.Background(), sip.Uri{User: "agent", Host: "127.0.0.1", Port: port}, body, sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatal(err)
	}

	var got100 bool
	var got180 bool
	answerCtx, cancelAnswer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAnswer()
	if err := dlg.WaitAnswer(answerCtx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode == 100 {
				got100 = true
			}
			if res.StatusCode == 180 {
				got180 = true
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	if err := dlg.Ack(context.Background()); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if !got100 || !got180 {
		t.Fatalf("expected 100 and 180, got100=%v got180=%v", got100, got180)
	}

	select {
	case <-byeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("expected BYE from agent")
	}
}

func TestInviteRejectedWhenAnswerDisabled(t *testing.T) {
	port := freeUDPPort(t)

	cfg := DefaultConfig()
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = port
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = port
	cfg.ContactParams = ""
	cfg.AnswerCalls = false
	cfg.SendMessages = false
	cfg.RejectStatus = 480
	cfg.RejectReason = "Temporarily Unavailable"
	cfg.AllowedCallers = nil
	cfg.RegisterURI = "sip:agent@127.0.0.1:5060"
	cfg.MessageURI = "sip:agent@127.0.0.1:5060"
	cfg.EntranceURI = "sip:agent@127.0.0.1:5060"

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
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

	body := sdp.GenerateForAudio(
		net.IPv4(127, 0, 0, 1),
		net.IPv4(127, 0, 0, 1),
		40000,
		sdp.ModeSendrecv,
		[]string{sdp.FORMAT_TYPE_ALAW},
	)

	host, portNum, err := sip.ParseAddr(clientAddr)
	if err != nil {
		t.Fatalf("parse client addr: %v", err)
	}
	dialogUA := &sipgo.DialogUA{
		Client: client,
		ContactHDR: sip.ContactHeader{
			Address: sip.Uri{
				User: "alice",
				Host: host,
				Port: portNum,
			},
		},
	}

	dlg, err := dialogUA.Invite(context.Background(), sip.Uri{User: "agent", Host: "127.0.0.1", Port: port}, body, sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatal(err)
	}

	answerCtx, cancelAnswer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAnswer()
	err = dlg.WaitAnswer(answerCtx, sipgo.AnswerOptions{})
	if err == nil {
		t.Fatal("expected non-2xx response")
	}

	if respErr, ok := err.(*sipgo.ErrDialogResponse); ok {
		if respErr.Res.StatusCode != 480 {
			t.Fatalf("expected 480, got %d", respErr.Res.StatusCode)
		}
	} else {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
}

func TestInviteForbiddenWhenCallerNotAllowed(t *testing.T) {
	port := freeUDPPort(t)

	cfg := DefaultConfig()
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = port
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = port
	cfg.ContactParams = ""
	cfg.AnswerCalls = true
	cfg.SendMessages = false
	cfg.AllowedCallers = []string{"alice"}
	cfg.RegisterURI = "sip:agent@127.0.0.1:5060"
	cfg.MessageURI = "sip:agent@127.0.0.1:5060"
	cfg.EntranceURI = "sip:agent@127.0.0.1:5060"

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
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

	body := sdp.GenerateForAudio(
		net.IPv4(127, 0, 0, 1),
		net.IPv4(127, 0, 0, 1),
		40000,
		sdp.ModeSendrecv,
		[]string{sdp.FORMAT_TYPE_ALAW},
	)

	host, portNum, err := sip.ParseAddr(clientAddr)
	if err != nil {
		t.Fatalf("parse client addr: %v", err)
	}
	dialogUA := &sipgo.DialogUA{
		Client: client,
		ContactHDR: sip.ContactHeader{
			Address: sip.Uri{
				User: "bob",
				Host: host,
				Port: portNum,
			},
		},
	}

	dlg, err := dialogUA.Invite(context.Background(), sip.Uri{User: "agent", Host: "127.0.0.1", Port: port}, body, sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatal(err)
	}

	answerCtx, cancelAnswer := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAnswer()
	err = dlg.WaitAnswer(answerCtx, sipgo.AnswerOptions{})
	if err == nil {
		t.Fatal("expected non-2xx response")
	}

	if respErr, ok := err.(*sipgo.ErrDialogResponse); ok {
		if respErr.Res.StatusCode != 403 {
			t.Fatalf("expected 403, got %d", respErr.Res.StatusCode)
		}
	} else {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
}
