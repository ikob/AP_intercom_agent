// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func dialogWithInvite(req *sip.Request) *diago.DialogServerSession {
	return &diago.DialogServerSession{
		DialogServerSession: &sipgo.DialogServerSession{
			Dialog: sipgo.Dialog{
				InviteRequest: req,
			},
		},
	}
}

func inviteWithFrom(user string, setFrom bool) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "agent", Host: "localhost"})
	if setFrom {
		req.AppendHeader(&sip.FromHeader{
			DisplayName: "Caller",
			Address: sip.Uri{
				User: user,
				Host: "example.com",
			},
		})
	}
	return req
}

func TestCallerAllowedWhitelistEmpty(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: nil}}
	inDialog := dialogWithInvite(nil)

	if !a.callerAllowedFromInvite(inDialog) {
		t.Fatal("expected allow when whitelist is empty")
	}
}

func TestCallerAllowedInviteMissing(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: []string{"alice"}}}
	inDialog := dialogWithInvite(nil)

	if a.callerAllowedFromInvite(inDialog) {
		t.Fatal("expected deny when invite is nil")
	}
}

func TestCallerAllowedFromMissing(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: []string{"alice"}}}
	req := inviteWithFrom("", false)

	if a.callerAllowedFromInvite(dialogWithInvite(req)) {
		t.Fatal("expected deny when From is missing")
	}
}

func TestCallerAllowedUserEmpty(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: []string{"alice"}}}
	req := inviteWithFrom("", true)

	if a.callerAllowedFromInvite(dialogWithInvite(req)) {
		t.Fatal("expected deny when From user is empty")
	}
}

func TestCallerAllowedUserNotWhitelisted(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: []string{"alice"}}}
	req := inviteWithFrom("bob", true)

	if a.callerAllowedFromInvite(dialogWithInvite(req)) {
		t.Fatal("expected deny when caller not in whitelist")
	}
}

func TestCallerAllowedUserWhitelisted(t *testing.T) {
	a := &Agent{cfg: Config{AllowedCallers: []string{"alice"}}}
	req := inviteWithFrom("alice", true)

	if !a.callerAllowedFromInvite(dialogWithInvite(req)) {
		t.Fatal("expected allow when caller is in whitelist")
	}
}

func TestNotifyRingWebhookDeduplicatesCallID(t *testing.T) {
	requests := make(chan RingWebhookEvent, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var event RingWebhookEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode webhook: %v", err)
		} else {
			requests <- event
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	agent := &Agent{
		cfg:             Config{RingWebhookURL: server.URL},
		ringWebhookSeen: make(map[string]time.Time),
	}

	req := inviteWithFrom("interphone1", true)
	callID := sip.CallIDHeader("ring-call-id")
	req.AppendHeader(&callID)
	dialog := dialogWithInvite(req)
	dialog.ID = "ring-dialog-id"

	if !agent.notifyRingWebhook(context.Background(), dialog, "interphone1") {
		t.Fatal("first ring webhook was not scheduled")
	}
	var got RingWebhookEvent
	select {
	case got = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ring webhook")
	}
	if got.Type != "ring" || got.Caller != "interphone1" || got.CallID != "ring-call-id" || got.DialogID != "ring-dialog-id" {
		t.Fatalf("unexpected ring webhook: %+v", got)
	}

	if agent.notifyRingWebhook(context.Background(), dialog, "interphone1") {
		t.Fatal("duplicate Call-ID scheduled a second ring webhook")
	}
	select {
	case duplicate := <-requests:
		t.Fatalf("received duplicate ring webhook: %+v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
}
