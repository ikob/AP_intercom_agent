// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"fmt"
	"image/color"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestBuildMonitorOffer(t *testing.T) {
	base := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 192.0.2.10\r\n" +
		"s=Sip Go Media\r\n" +
		"c=IN IP4 192.0.2.10\r\n" +
		"t=0 0\r\n" +
		"m=audio 20000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=ptime:20\r\n" +
		"a=maxptime:20\r\n" +
		"a=sendrecv\r\n")

	offer, err := buildMonitorOffer(base, "cellphone0")
	if err != nil {
		t.Fatal(err)
	}
	got := string(offer)
	for _, want := range []string{
		"o=cellphone0 1 1 IN IP4 192.0.2.10\r\n",
		"s=session\r\n",
		"m=audio 20000 RTP/AVP 0\r\n",
		"m=video 8080 HTTP jpeg\r\n",
		"a=recvonly\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("offer does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "a=ptime:") || strings.Contains(got, "a=maxptime:") {
		t.Fatalf("offer contains Diago-only timing attributes:\n%s", got)
	}
}

func TestReliableSequence(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "monitor", Host: "example.test"})
	res := sip.NewResponseFromRequest(req, sip.StatusRinging, "Ringing", nil)
	res.AppendHeader(sip.NewHeader("Require", "timer, 100rel"))
	res.AppendHeader(sip.NewHeader("RSeq", "1000"))

	got, reliable, err := reliableSequence(res)
	if err != nil {
		t.Fatal(err)
	}
	if !reliable || got != 1000 {
		t.Fatalf("expected reliable RSeq 1000, got reliable=%v rseq=%d", reliable, got)
	}
}

func TestMonitorDialogSequenceTwice(t *testing.T) {
	serverPort := freeUDPPort(t)
	agentPort := freeUDPPort(t)

	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: serverPort})
	if err != nil {
		t.Fatal(err)
	}
	serverUA, err := sipgo.NewUA(sipgo.WithUserAgent("INTERPHONE_IFBOX"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := sipgo.NewServer(serverUA)
	if err != nil {
		t.Fatal(err)
	}

	var invites atomic.Int32
	var pracks atomic.Int32
	var byes atomic.Int32
	var registers atomic.Int32
	var jpegRequests atomic.Int32
	var authenticatedCSeq atomic.Uint32
	prackComplete := make(chan struct{}, 2)
	serverErrors := make(chan error, 16)
	jpegData := testJPEG(t, color.RGBA{R: 40, G: 80, B: 120, A: 255})
	jpegServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Host != monitorJPEGHost {
			serverErrors <- fmt.Errorf("unexpected JPEG Host: %q", request.Host)
		}
		if request.URL.Path != monitorJPEGPath || request.URL.Query().Get("s") != defaultJPEGQueryS {
			serverErrors <- fmt.Errorf("unexpected JPEG URL: %s", request.URL.Redacted())
		}
		jpegRequests.Add(1)
		response.Header().Set("Content-Type", "image/jpeg")
		_, _ = response.Write(jpegData)
	}))
	t.Cleanup(jpegServer.Close)
	jpegPort := jpegServer.Listener.Addr().(*net.TCPAddr).Port

	contactValue := fmt.Sprintf("<sip:monitor@127.0.0.1:%d>", serverPort)
	recordRouteValue := fmt.Sprintf("<sip:127.0.0.1:%d;lr>", serverPort)
	setDialogHeaders := func(res *sip.Response, tag string) {
		res.To().Params.Add("tag", tag)
		res.AppendHeader(sip.NewHeader("Contact", contactValue))
		res.AppendHeader(sip.NewHeader("Record-Route", recordRouteValue))
	}

	server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		iteration := (invites.Add(1) + 1) / 2
		if req.GetHeader("Proxy-Authorization") == nil {
			res := sip.NewResponseFromRequest(req, sip.StatusProxyAuthRequired, "Proxy Authentication Required", nil)
			res.AppendHeader(sip.NewHeader("Proxy-Authenticate", `Digest realm="ifbox", nonce="monitor-test", algorithm=MD5`))
			if err := tx.Respond(res); err != nil {
				serverErrors <- fmt.Errorf("respond 407: %w", err)
				return
			}
			select {
			case <-tx.Acks():
			case <-time.After(2 * time.Second):
				serverErrors <- fmt.Errorf("timeout waiting for 407 ACK")
			}
			return
		}

		if !strings.Contains(string(req.Body()), "m=video 8080 HTTP jpeg\r\na=recvonly\r\n") {
			serverErrors <- fmt.Errorf("authenticated INVITE has unexpected SDP: %q", string(req.Body()))
		}
		if h := req.GetHeader("Supported"); h == nil || !headerHasToken(h.Value(), "100rel") {
			serverErrors <- fmt.Errorf("authenticated INVITE has no Supported: 100rel")
		}
		if cseq := req.CSeq(); cseq != nil {
			authenticatedCSeq.Store(cseq.SeqNo)
		} else {
			serverErrors <- fmt.Errorf("authenticated INVITE has no CSeq")
		}

		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusTrying, "Trying", nil)); err != nil {
			serverErrors <- fmt.Errorf("respond 100: %w", err)
			return
		}
		tag := fmt.Sprintf("monitor-%d", iteration)
		ringing := sip.NewResponseFromRequest(req, sip.StatusRinging, "Ringing", nil)
		setDialogHeaders(ringing, tag)
		ringing.AppendHeader(sip.NewHeader("Require", "100rel"))
		ringing.AppendHeader(sip.NewHeader("RSeq", "1000"))
		if err := tx.Respond(ringing); err != nil {
			serverErrors <- fmt.Errorf("respond 180: %w", err)
			return
		}

		select {
		case <-prackComplete:
		case <-time.After(2 * time.Second):
			serverErrors <- fmt.Errorf("timeout waiting for PRACK")
			return
		}

		answerBody := []byte("v=0\r\n" +
			"o=monitor 2 2 IN IP4 127.0.0.1\r\n" +
			"s=session\r\n" +
			"c=IN IP4 127.0.0.1\r\n" +
			"t=0 0\r\n" +
			"m=audio 21000 RTP/AVP 0\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n" +
			"a=sendrecv\r\n" +
			fmt.Sprintf("m=video %d HTTP jpeg\r\n", jpegPort) +
			"a=sendrecv\r\n")
		ok := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", answerBody)
		setDialogHeaders(ok, tag)
		ok.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		if err := tx.Respond(ok); err != nil {
			serverErrors <- fmt.Errorf("respond INVITE 200: %w", err)
			return
		}
		// A 2xx ACK is a separate transaction. The raw test UAS does not own a
		// dialog cache, so terminate this response transaction after sending it.
		tx.Terminate()
	})
	server.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		registers.Add(1)
		if req.GetHeader("Authorization") == nil {
			res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="ifbox", nonce="register-test", algorithm=MD5`))
			if err := tx.Respond(res); err != nil {
				serverErrors <- fmt.Errorf("respond REGISTER 401: %w", err)
			}
			return
		}
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			serverErrors <- fmt.Errorf("respond REGISTER 200: %w", err)
		}
	})

	server.OnPrack(func(req *sip.Request, tx sip.ServerTransaction) {
		want := fmt.Sprintf("1000 %d INVITE", authenticatedCSeq.Load())
		if rack := req.GetHeader("RAck"); rack == nil || rack.Value() != want {
			serverErrors <- fmt.Errorf("unexpected RAck: got %v, want %q", rack, want)
		}
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			serverErrors <- fmt.Errorf("respond PRACK 200: %w", err)
			return
		}
		pracks.Add(1)
		prackComplete <- struct{}{}
	})
	server.OnAck(func(_ *sip.Request, _ sip.ServerTransaction) {})

	server.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		wantCSeq := authenticatedCSeq.Load() + 2 // PRACK is +1, BYE is +2.
		if cseq := req.CSeq(); cseq == nil || cseq.SeqNo != wantCSeq {
			serverErrors <- fmt.Errorf("unexpected BYE CSeq: got %v, want %d", cseq, wantCSeq)
		}
		if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
			serverErrors <- fmt.Errorf("respond BYE 200: %w", err)
			return
		}
		byes.Add(1)
	})

	cfg := testConfig(agentPort)
	cfg.UserAgentHostname = "127.0.0.1"
	cfg.ContactParams = ";+l.operator.docomo.intercom"
	cfg.RegisterURI = fmt.Sprintf("sip:alice@127.0.0.1:%d", serverPort)
	cfg.MonitorProbe = true
	cfg.MonitorURI = fmt.Sprintf("sip:monitor@127.0.0.1:%d", serverPort)
	cfg.MonitorHold = Duration{Duration: 120 * time.Millisecond}
	cfg.MonitorRepeat = 2
	cfg.MonitorJPEGOut = filepath.Join(t.TempDir(), "monitor.jpg")
	cfg.MonitorJPEGInterval = Duration{Duration: 40 * time.Millisecond}
	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)

	go func() {
		if err := serverUA.TransportLayer().ServeUDP(listener); err != nil && !strings.Contains(err.Error(), "closed") {
			serverErrors <- fmt.Errorf("serve UDP: %w", err)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = server.Close()
		_ = serverUA.Close()
	})

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	err = agent.RunMonitorProbe(probeCtx)
	cancelProbe()
	if err != nil {
		t.Fatalf("RunMonitorProbe: %v", err)
	}

	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
	if got := invites.Load(); got != 4 {
		t.Fatalf("expected 4 INVITEs (challenge + authenticated, twice), got %d", got)
	}
	if got := pracks.Load(); got != 2 {
		t.Fatalf("expected 2 PRACKs, got %d", got)
	}
	if got := byes.Load(); got != 2 {
		t.Fatalf("expected 2 BYEs, got %d", got)
	}
	if got := registers.Load(); got != 4 {
		t.Fatalf("expected 4 REGISTERs (register and unregister, each challenged), got %d", got)
	}
	if got := jpegRequests.Load(); got < 2 {
		t.Fatalf("expected JPEG polling in both dialogs, got %d requests", got)
	}
	if info, err := os.Stat(cfg.MonitorJPEGOut); err != nil || info.Size() == 0 {
		t.Fatalf("monitor JPEG output was not written: info=%v err=%v", info, err)
	}
}
