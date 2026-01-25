// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

// agent_invite_test.go
package main

import (
	"context"
	"net"
	"testing"

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

	client, clientAddr, cleanup := startUAC(t)
	defer cleanup()

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "agent", Host: "127.0.0.1", Port: port})
	req.AppendHeader(sip.NewHeader("Contact", "<sip:alice@"+clientAddr+">"))

	// Minimal SDP.
	body := sdp.GenerateForAudio(
		net.IPv4(127, 0, 0, 1),
		net.IPv4(127, 0, 0, 1),
		40000,
		sdp.ModeSendrecv,
		[]string{sdp.FORMAT_TYPE_ALAW},
	)
	req.SetBody(body)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	tx, err := client.TransactionRequest(context.Background(), req, sipgo.ClientRequestBuild)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Terminate()

	res := waitFinalResponse(t, tx)
	if res.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
}
