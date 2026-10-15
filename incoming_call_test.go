// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

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

func TestManualAnswerAndHangupSIPLifecycle(t *testing.T) {
	agentPort := freeUDPPort(t)
	cfg := DefaultConfig()
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = agentPort
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = agentPort
	cfg.ContactParams = ""
	cfg.AnswerCalls = false
	cfg.CaptureImages = false
	cfg.SendMessages = false
	cfg.AllowedCallers = []string{"interphone0"}
	cfg.RegisterURI = "sip:agent@127.0.0.1:5060"
	cfg.MessageURI = "sip:agent@127.0.0.1:5060"
	cfg.EntranceURI = "sip:agent@127.0.0.1:5060"

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}
	agent.incomingCallTimeout = 3 * time.Second
	agent.registerLoopFn = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		agent.Close()
	})
	go func() { _ = agent.Run(ctx) }()

	bye := make(chan *sip.Request, 1)
	client, clientAddress, cleanupClient := startUACWithBye(t, bye)
	t.Cleanup(cleanupClient)
	host, port, err := sip.ParseAddr(clientAddress)
	if err != nil {
		t.Fatal(err)
	}
	dialogUA := &sipgo.DialogUA{
		Client: client,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{
			Scheme: "sip",
			User:   "interphone0",
			Host:   host,
			Port:   port,
		}},
	}
	offer := sdp.GenerateForAudio(
		net.IPv4(127, 0, 0, 1),
		net.IPv4(127, 0, 0, 1),
		21000,
		sdp.ModeSendrecv,
		[]string{sdp.FORMAT_TYPE_ULAW},
	)
	dialog, err := dialogUA.Invite(context.Background(),
		sip.Uri{Scheme: "sip", User: "cellphone0", Host: "127.0.0.1", Port: agentPort},
		offer,
		sip.NewHeader("From", `"interphone0" <sip:interphone0@127.0.0.1>;tag=manual-call`),
		sip.NewHeader("Content-Type", "application/sdp"),
	)
	if err != nil {
		t.Fatal(err)
	}

	answerResult := make(chan error, 1)
	go func() {
		answerCtx, cancelAnswer := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelAnswer()
		answerResult <- dialog.WaitAnswer(answerCtx, sipgo.AnswerOptions{})
	}()

	snapshot := waitForCallState(t, agent, CallStateRinging)
	if err := agent.enqueueIncomingCallCommand(snapshot.SessionID, incomingCallAnswer); err != nil {
		t.Fatalf("queue answer: %v", err)
	}
	if err := <-answerResult; err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	if err := dialog.Ack(context.Background()); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForCallState(t, agent, CallStateConnected)
	if err := agent.enqueueIncomingCallCommand(snapshot.SessionID, incomingCallHangup); err != nil {
		t.Fatalf("queue hangup: %v", err)
	}
	select {
	case <-bye:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not send BYE")
	}
	waitForCallState(t, agent, CallStateIdle)
}

func waitForCallState(t *testing.T, agent *Agent, want CallState) CallSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := agent.currentCallSnapshot()
		if snapshot.State == want {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshot := agent.currentCallSnapshot()
	t.Fatalf("call state = %s, want %s (snapshot=%+v)", snapshot.State, want, snapshot)
	return CallSnapshot{}
}
