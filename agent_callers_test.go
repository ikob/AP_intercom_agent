// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"testing"

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
