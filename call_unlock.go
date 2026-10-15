// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"log/slog"
)

// incomingCallUnlock records the unlock state for one inbound SIP dialog.
// A MESSAGE transaction is complete when its final 2xx response arrives; SIP
// MESSAGE does not use a separate ACK request.
type incomingCallUnlock struct {
	appCtx    context.Context
	dialogCtx context.Context
	dialogID  string
	callID    string
	caller    string
	eligible  bool
	ready     bool
	attempted bool
	confirmed bool
}

func (a *Agent) beginIncomingCallUnlock(appCtx, dialogCtx context.Context, dialogID, callID, caller string) *incomingCallUnlock {
	call := &incomingCallUnlock{
		appCtx:    appCtx,
		dialogCtx: dialogCtx,
		dialogID:  dialogID,
		callID:    callID,
		caller:    caller,
		eligible:  a.unlockCallerAllowed(caller),
	}

	a.callUnlockMu.Lock()
	if a.callUnlocks == nil {
		a.callUnlocks = make(map[*incomingCallUnlock]struct{})
	}
	a.callUnlocks[call] = struct{}{}
	a.callUnlockMu.Unlock()
	return call
}

func (a *Agent) endIncomingCallUnlock(call *incomingCallUnlock) {
	if call == nil {
		return
	}
	a.callUnlockMu.Lock()
	delete(a.callUnlocks, call)
	a.callUnlockMu.Unlock()
}

func (a *Agent) markIncomingCallUnlockReady(call *incomingCallUnlock) {
	if call == nil {
		return
	}
	a.callUnlockMu.Lock()
	if _, active := a.callUnlocks[call]; active {
		call.ready = true
	}
	a.callUnlockMu.Unlock()
	a.tryIncomingCallUnlock(call)
}

// triggerActiveCallUnlocks is deliberately asynchronous. The Home Assistant
// REST switch must not wait for a SIP MESSAGE transaction, which can take
// longer than its HTTP timeout.
func (a *Agent) triggerActiveCallUnlocks() {
	a.callUnlockMu.Lock()
	calls := make([]*incomingCallUnlock, 0, len(a.callUnlocks))
	for call := range a.callUnlocks {
		calls = append(calls, call)
	}
	a.callUnlockMu.Unlock()

	for _, call := range calls {
		go a.tryIncomingCallUnlock(call)
	}
}

func (a *Agent) tryIncomingCallUnlock(call *incomingCallUnlock) {
	if call == nil {
		return
	}

	a.callUnlockMu.Lock()
	_, active := a.callUnlocks[call]
	if !active || !call.ready || !call.eligible || call.attempted || !a.messageEnabled.Load() || call.appCtx.Err() != nil || call.dialogCtx.Err() != nil {
		a.callUnlockMu.Unlock()
		return
	}
	// Claim the only send attempt before releasing the lock. Concurrent state
	// updates and call progress callbacks therefore cannot duplicate it.
	call.attempted = true
	a.callUnlockMu.Unlock()

	err := a.sendMessage(call.dialogCtx, a.entranceRecipient, a.cfg.EntranceContentType, []byte(a.cfg.EntranceBody))
	if err != nil {
		slog.Error("Failed to send entrance MESSAGE",
			"dialog_id", call.dialogID,
			"call_id", call.callID,
			"caller", call.caller,
			"error", err,
		)
		return
	}

	a.callUnlockMu.Lock()
	call.confirmed = true
	a.callUnlockMu.Unlock()
	slog.Info("Entrance unlock MESSAGE acknowledged",
		"dialog_id", call.dialogID,
		"call_id", call.callID,
		"caller", call.caller,
	)
}
