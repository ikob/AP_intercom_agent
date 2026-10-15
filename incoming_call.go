// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emiago/diago"
)

const defaultIncomingCallTimeout = 30 * time.Second

type CallState string

const (
	CallStateIdle      CallState = "idle"
	CallStateRinging   CallState = "ringing"
	CallStateAnswering CallState = "answering"
	CallStateConnected CallState = "connected"
	CallStateEnding    CallState = "ending"
)

type CallSnapshot struct {
	State         CallState  `json:"state"`
	MonitorActive bool       `json:"monitor_active"`
	SessionID     string     `json:"session_id,omitempty"`
	Caller        string     `json:"caller,omitempty"`
	CallID        string     `json:"call_id,omitempty"`
	DialogID      string     `json:"dialog_id,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

type incomingCallCommand uint8

const (
	incomingCallAnswer incomingCallCommand = iota + 1
	incomingCallHangup
)

type incomingCallSession struct {
	sessionID string
	caller    string
	callID    string
	dialogID  string
	dialog    *diago.DialogServerSession
	commands  chan incomingCallCommand
}

var (
	errNoActiveCall      = errors.New("no active incoming call")
	errStaleCallSession  = errors.New("call session no longer active")
	errCallActionInvalid = errors.New("call action is not valid in the current state")
	errCallActionPending = errors.New("another call action is pending")
)

func (a *Agent) handleIncomingDialog(appCtx context.Context, dialog *diago.DialogServerSession) {
	slog.Info("New dialog request", "id", dialog.ID)
	defer slog.Info("Dialog finished", "id", dialog.ID)

	_ = dialog.Trying()
	slog.Info("Trying", "id", dialog.ID)
	if !a.callerAllowedFromInvite(dialog) {
		_ = dialog.Respond(403, "Forbidden", nil)
		return
	}

	caller := callerFromInvite(dialog)
	a.notifyRingWebhook(appCtx, dialog, caller)
	session, ok := a.beginIncomingCall(dialog, caller)
	if !ok {
		_ = dialog.Respond(486, "Busy Here", nil)
		return
	}
	endReason := "completed"
	defer func() {
		a.finishIncomingCall(appCtx, session, endReason)
		if a.cameraMonitor != nil {
			a.cameraMonitor.Resume()
		}
	}()

	// An incoming call owns the single IFBOX SIP media slot. Preserve camera
	// demand, stop its outgoing dialog, and let it resume after this call ends.
	if a.cameraMonitor != nil {
		a.cameraMonitor.Suspend()
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), cameraMonitorStopTimeout)
	if err := a.StopMonitor(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("Monitor preemption did not complete before incoming call", "error", err)
	}
	cancelStop()

	unlockCall := a.beginIncomingCallUnlock(appCtx, dialog.Context(), dialog.ID, session.callID, caller)
	defer a.endIncomingCallUnlock(unlockCall)
	if a.messageEnabled.Load() && !unlockCall.eligible {
		slog.Info("Automatic unlock skipped for caller", "caller", caller)
	}
	markUnlockReady := func() { a.markIncomingCallUnlockReady(unlockCall) }

	media, err := a.prepareIncomingCall(appCtx, dialog, a.shouldCaptureIncomingJPEG(a.captureEnabled.Load()), markUnlockReady)
	if err != nil {
		markUnlockReady()
		if dialog.Context().Err() == nil {
			_ = dialog.Respond(a.cfg.RejectStatus, a.cfg.RejectReason, nil)
			endReason = "setup_error"
		} else {
			endReason = "remote_cancel"
		}
		slog.Error("Failed to prepare incoming call", "id", dialog.ID, "error", err)
		return
	}
	defer media.Close()

	autoAnswer := a.answerEnabled.Load()
	if autoAnswer {
		session.commands <- incomingCallAnswer
	}
	endReason = a.runIncomingCallSession(appCtx, session, media, autoAnswer)
}

func (a *Agent) beginIncomingCall(dialog *diago.DialogServerSession, caller string) (*incomingCallSession, bool) {
	callID := sipRequestCallID(dialog.InviteRequest)
	sessionID := strings.TrimSpace(dialog.ID)
	if sessionID == "" {
		sessionID = callID
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("incoming-%d", time.Now().UnixNano())
	}
	session := &incomingCallSession{
		sessionID: sessionID,
		caller:    caller,
		callID:    callID,
		dialogID:  dialog.ID,
		dialog:    dialog,
		commands:  make(chan incomingCallCommand, 2),
	}
	now := time.Now()
	a.callMu.Lock()
	if a.activeCall != nil {
		a.callMu.Unlock()
		return nil, false
	}
	a.activeCall = session
	previous := a.callSnapshot.State
	if previous == "" {
		previous = CallStateIdle
	}
	a.callSnapshot = CallSnapshot{
		State:     CallStateRinging,
		SessionID: sessionID,
		Caller:    caller,
		CallID:    callID,
		DialogID:  dialog.ID,
		StartedAt: &now,
		UpdatedAt: &now,
	}
	snapshot := a.callSnapshot
	a.callMu.Unlock()
	a.notifyCallStateWebhook(context.Background(), previous, snapshot)
	return session, true
}

func (a *Agent) hasActiveIncomingCall() bool {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	return a.activeCall != nil
}

func (a *Agent) currentCallSnapshot() CallSnapshot {
	a.callMu.Lock()
	snapshot := a.callSnapshot
	a.callMu.Unlock()
	if snapshot.State == "" {
		snapshot.State = CallStateIdle
	}
	snapshot.MonitorActive = a.monitorActive()
	return snapshot
}

func (a *Agent) transitionIncomingCall(ctx context.Context, session *incomingCallSession, state CallState, reason string) bool {
	a.callMu.Lock()
	if a.activeCall != session {
		a.callMu.Unlock()
		return false
	}
	previous := a.callSnapshot.State
	if previous == state && a.callSnapshot.Reason == reason {
		a.callMu.Unlock()
		return true
	}
	a.callSnapshot.State = state
	now := time.Now()
	a.callSnapshot.UpdatedAt = &now
	a.callSnapshot.Reason = reason
	snapshot := a.callSnapshot
	a.callMu.Unlock()
	slog.Info("Incoming call state changed", "session_id", session.sessionID, "previous_state", previous, "state", state, "reason", reason)
	a.notifyCallStateWebhook(ctx, previous, snapshot)
	return true
}

func (a *Agent) finishIncomingCall(ctx context.Context, session *incomingCallSession, reason string) {
	a.callMu.Lock()
	if a.activeCall != session {
		a.callMu.Unlock()
		return
	}
	previous := a.callSnapshot.State
	ended := a.callSnapshot
	ended.State = CallStateIdle
	now := time.Now()
	ended.UpdatedAt = &now
	ended.Reason = reason
	a.activeCall = nil
	a.callSnapshot = CallSnapshot{State: CallStateIdle, UpdatedAt: &now, Reason: reason}
	a.callMu.Unlock()
	slog.Info("Incoming call ended", "session_id", session.sessionID, "previous_state", previous, "reason", reason)
	a.notifyCallStateWebhook(ctx, previous, ended)
}

func (a *Agent) enqueueIncomingCallCommand(sessionID string, command incomingCallCommand) error {
	a.callMu.Lock()
	session := a.activeCall
	snapshot := a.callSnapshot
	if session == nil {
		a.callMu.Unlock()
		return errNoActiveCall
	}
	if session.sessionID != sessionID {
		a.callMu.Unlock()
		return errStaleCallSession
	}
	valid := false
	switch command {
	case incomingCallAnswer:
		valid = snapshot.State == CallStateRinging
	case incomingCallHangup:
		valid = snapshot.State == CallStateRinging || snapshot.State == CallStateAnswering || snapshot.State == CallStateConnected
	}
	if !valid {
		a.callMu.Unlock()
		return errCallActionInvalid
	}
	select {
	case session.commands <- command:
		a.callMu.Unlock()
		return nil
	default:
		a.callMu.Unlock()
		return errCallActionPending
	}
}

func (a *Agent) runIncomingCallSession(appCtx context.Context, session *incomingCallSession, media *incomingCallMedia, autoAnswer bool) string {
	timeout := a.incomingCallTimeout
	if timeout <= 0 {
		timeout = defaultIncomingCallTimeout
	}
	ringTimer := time.NewTimer(timeout)
	defer ringTimer.Stop()

	answerResult := make(chan error, 1)
	hangupResult := make(chan error, 1)
	var autoHangup <-chan time.Time
	answering := false
	hangupStarted := false
	hangupAfterAnswer := false

	startHangup := func() {
		if hangupStarted {
			return
		}
		hangupStarted = true
		a.transitionIncomingCall(appCtx, session, CallStateEnding, "local_hangup")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), cameraMonitorStopTimeout)
			err := session.dialog.Hangup(ctx)
			cancel()
			hangupResult <- err
		}()
	}

	for {
		select {
		case command := <-session.commands:
			switch command {
			case incomingCallAnswer:
				if answering || a.currentCallSnapshot().State != CallStateRinging {
					continue
				}
				answering = true
				if !ringTimer.Stop() {
					select {
					case <-ringTimer.C:
					default:
					}
				}
				a.transitionIncomingCall(appCtx, session, CallStateAnswering, "")
				go func() { answerResult <- session.dialog.DialogServerSession.RespondSDP(media.answer) }()
			case incomingCallHangup:
				state := a.currentCallSnapshot().State
				switch state {
				case CallStateRinging:
					a.transitionIncomingCall(appCtx, session, CallStateEnding, "local_reject")
					_ = session.dialog.Respond(a.cfg.RejectStatus, a.cfg.RejectReason, nil)
					return "local_reject"
				case CallStateAnswering:
					hangupAfterAnswer = true
					a.transitionIncomingCall(appCtx, session, CallStateEnding, "local_hangup")
				case CallStateConnected:
					startHangup()
				}
			}

		case err := <-answerResult:
			if err != nil {
				if session.dialog.Context().Err() != nil {
					return "remote_cancel"
				}
				slog.Warn("Incoming call answer failed", "session_id", session.sessionID, "error", err)
				return "answer_failed"
			}
			if hangupAfterAnswer {
				startHangup()
				continue
			}
			a.transitionIncomingCall(appCtx, session, CallStateConnected, "")
			if autoAnswer {
				autoHangup = time.After(time.Second)
			}

		case err := <-hangupResult:
			if err != nil && session.dialog.Context().Err() == nil {
				slog.Debug("Incoming call hangup failed", "session_id", session.sessionID, "error", err)
				return "hangup_failed"
			}
			return "local_hangup"

		case <-autoHangup:
			startHangup()
			autoHangup = nil

		case <-ringTimer.C:
			a.transitionIncomingCall(appCtx, session, CallStateEnding, "ring_timeout")
			_ = session.dialog.Respond(a.cfg.RejectStatus, a.cfg.RejectReason, nil)
			return "ring_timeout"

		case <-session.dialog.Context().Done():
			if hangupStarted {
				return "local_hangup"
			}
			state := a.currentCallSnapshot().State
			if state == CallStateConnected || state == CallStateEnding {
				return "remote_bye"
			}
			return "remote_cancel"

		case <-appCtx.Done():
			return "shutdown"
		}
	}
}
