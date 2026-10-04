// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port
}

func startUAC(t *testing.T) (*sipgo.Client, string, func()) {
	return startUACWithBye(t, nil)
}

func startUACWithBye(t *testing.T, byeCh chan<- *sip.Request) (*sipgo.Client, string, func()) {
	t.Helper()

	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}

	client, err := sipgo.NewClient(ua, sipgo.WithClientAddr(l.LocalAddr().String()))
	if err != nil {
		t.Fatal(err)
	}

	// Start a UAS to respond 200 OK to BYE from the agent.
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if byeCh != nil {
			byeCh <- req
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	go ua.TransportLayer().ServeUDP(l)

	cleanup := func() {
		_ = l.Close()
		_ = srv.Close()
		_ = ua.Close()
	}

	return client, l.LocalAddr().String(), cleanup
}

func waitFinalResponse(t *testing.T, tx sip.ClientTransaction) *sip.Response {
	t.Helper()

	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case res := <-tx.Responses():
			if res == nil {
				t.Fatal("nil response")
			}
			if res.StatusCode >= 200 {
				return res
			}
		case <-timeout.C:
			t.Fatal("timeout waiting for final response")
		}
	}
}
