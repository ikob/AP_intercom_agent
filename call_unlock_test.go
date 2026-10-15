// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type blockingUnlockClient struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (client *blockingUnlockClient) Do(ctx context.Context, request *sip.Request, _ ...sipgo.ClientRequestOption) (*sip.Response, error) {
	client.calls.Add(1)
	select {
	case client.started <- struct{}{}:
	default:
	}
	select {
	case <-client.release:
		return sip.NewResponseFromRequest(request, 200, "OK", nil), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*blockingUnlockClient) DoDigestAuth(context.Context, *sip.Request, *sip.Response, sipgo.DigestAuth) (*sip.Response, error) {
	return nil, errors.New("unexpected digest authentication")
}

func (*blockingUnlockClient) Close() error { return nil }

func TestActiveCallUnlocksOnceAndNextCallUnlocksAgain(t *testing.T) {
	cfg := testConfig(freeUDPPort(t))
	cfg.UnlockCallers = []string{"interphone0"}
	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)
	client := &blockingUnlockClient{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	agent.cli = client

	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	dialogCtx, cancelDialog := context.WithCancel(context.Background())
	call := agent.beginIncomingCallUnlock(appCtx, dialogCtx, "dialog-1", "call-1", "interphone0")
	agent.markIncomingCallUnlockReady(call)
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("unlock attempts while disabled = %d, want 0", got)
	}

	// Enabling during the active call starts one MESSAGE transaction.
	agent.SetMessageEnabled(true)
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("unlock MESSAGE did not start after enabling during the call")
	}

	// Repeated ON updates while waiting for the final response must not start a
	// second transaction for the same call.
	for range 5 {
		agent.SetMessageEnabled(true)
	}
	time.Sleep(20 * time.Millisecond)
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("unlock attempts before acknowledgement = %d, want 1", got)
	}

	close(client.release)
	confirmed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		agent.callUnlockMu.Lock()
		confirmed = call.confirmed
		agent.callUnlockMu.Unlock()
		if confirmed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !confirmed {
		t.Fatal("unlock MESSAGE was not marked acknowledged")
	}
	agent.SetMessageEnabled(false)
	agent.SetMessageEnabled(true)
	time.Sleep(20 * time.Millisecond)
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("unlock attempts after acknowledgement = %d, want 1", got)
	}

	agent.endIncomingCallUnlock(call)
	cancelDialog()

	// The switch remains ON, but a new call gets its own one-time unlock.
	nextDialogCtx, cancelNextDialog := context.WithCancel(context.Background())
	defer cancelNextDialog()
	nextCall := agent.beginIncomingCallUnlock(appCtx, nextDialogCtx, "dialog-2", "call-2", "interphone0")
	defer agent.endIncomingCallUnlock(nextCall)
	agent.markIncomingCallUnlockReady(nextCall)
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("unlock attempts after second call = %d, want 2", got)
	}

	// An active call outside unlock_callers remains locked even while ON.
	excludedCtx, cancelExcluded := context.WithCancel(context.Background())
	defer cancelExcluded()
	excluded := agent.beginIncomingCallUnlock(appCtx, excludedCtx, "dialog-3", "call-3", "interphone1")
	defer agent.endIncomingCallUnlock(excluded)
	agent.markIncomingCallUnlockReady(excluded)
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("unlock attempts after excluded caller = %d, want 2", got)
	}
}
