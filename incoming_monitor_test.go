// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"fmt"
	"image/color"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/siptest"
)

type alwaysOKSIPClient struct {
	unlockCalls atomic.Int32
}

func (client *alwaysOKSIPClient) Do(_ context.Context, request *sip.Request, _ ...sipgo.ClientRequestOption) (*sip.Response, error) {
	if strings.Contains(string(request.Body()), "UNLOCK") {
		client.unlockCalls.Add(1)
	}
	return sip.NewResponseFromRequest(request, http.StatusOK, "OK", nil), nil
}

func (*alwaysOKSIPClient) DoDigestAuth(_ context.Context, request *sip.Request, _ *sip.Response, _ sipgo.DigestAuth) (*sip.Response, error) {
	return sip.NewResponseFromRequest(request, http.StatusOK, "OK", nil), nil
}

func (*alwaysOKSIPClient) Close() error { return nil }

func TestBuildIncomingMonitorAnswer(t *testing.T) {
	answer, audioSocket, err := buildIncomingMonitorAnswer("127.0.0.1", "cellphone0")
	if err != nil {
		t.Fatal(err)
	}
	defer audioSocket.Close()
	port := audioSocket.LocalAddr().(*net.UDPAddr).Port
	got := string(answer)
	for _, want := range []string{
		"o=cellphone0 ",
		"c=IN IP4 127.0.0.1\r\n",
		"m=audio " + asDecimal(port) + " RTP/AVP 0\r\n",
		"a=rtpmap:0 PCMU/8000\r\n",
		"m=video 8080 HTTP jpeg\r\n",
		"a=recvonly\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("answer does not contain %q:\n%s", want, got)
		}
	}
}

func TestHandleInboundPRACKAcceptsIFBOXQuirks(t *testing.T) {
	agent := &Agent{prackWaiters: make(map[string]*inboundPRACKWaiter)}
	waiter, err := agent.registerPRACKWaiter("call-1", "our-tag")
	if err != nil {
		t.Fatal(err)
	}

	request := sip.NewRequest(sip.PRACK, sip.Uri{User: "cellphone0", Host: "127.0.0.1", Port: 15060})
	request.AppendHeader(sip.NewHeader("Via", "SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK-prack"))
	request.AppendHeader(sip.NewHeader("From", `"interphone1" <sip:interphone1@127.0.0.1>;tag=changed-tag`))
	request.AppendHeader(sip.NewHeader("To", "<sip:cellphone0@127.0.0.1>;tag=our-tag"))
	request.AppendHeader(sip.NewHeader("Call-ID", "call-1"))
	request.AppendHeader(sip.NewHeader("CSeq", "4 PRACK"))
	request.AppendHeader(sip.NewHeader("RAck", "(null)2 INVITE"))
	request.SetBody(nil)
	tx := siptest.NewServerTxRecorder(request)

	agent.handleInboundPRACK(request, tx)

	select {
	case <-waiter.done:
	case <-time.After(time.Second):
		t.Fatal("PRACK did not release waiter")
	}
	responses := tx.Result()
	if len(responses) != 1 || responses[0].StatusCode != 200 {
		t.Fatalf("unexpected PRACK responses: %v", responses)
	}
}

func TestValidateIncomingJPEGEndpoint(t *testing.T) {
	request := sip.NewRequest(sip.INVITE, sip.Uri{User: "cellphone0", Host: "127.0.0.1"})
	request.SetSource("192.0.2.25:5060")
	matching, _ := url.Parse("http://192.0.2.25:8080" + monitorJPEGPath)
	if err := validateIncomingJPEGEndpoint(request, matching); err != nil {
		t.Fatal(err)
	}
	nonmatching, _ := url.Parse("http://192.0.2.26:8080" + monitorJPEGPath)
	if err := validateIncomingJPEGEndpoint(request, nonmatching); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected endpoint mismatch, got %v", err)
	}
}

func TestSafeIncomingCaller(t *testing.T) {
	for input, want := range map[string]string{
		"interphone0":      "interphone0",
		"north/../../door": "north_______door",
		" entrance three ": "entrance_three",
		"...":              "unknown",
	} {
		if got := safeIncomingCaller(input); got != want {
			t.Errorf("safeIncomingCaller(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIncomingJPEGFilename(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	at := time.Date(2026, time.October, 4, 21, 41, 23, 123456789, jst)
	got := incomingJPEGFilename(at, "north/door")
	want := "20261004T214123.123456789+0900_north_door.jpg"
	if got != want {
		t.Fatalf("filename = %q, want %q", got, want)
	}
}

func TestIncomingJPEGCaptureAndAutomaticUnlockModes(t *testing.T) {
	agentPort := freeUDPPort(t)
	jpegData := testJPEG(t, color.RGBA{R: 30, G: 90, B: 150, A: 255})
	var jpegRequests atomic.Int32
	jpegServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Host != monitorJPEGHost {
			t.Errorf("Host = %q, want %q", request.Host, monitorJPEGHost)
		}
		if request.URL.Path != monitorJPEGPath || request.URL.Query().Get("s") != defaultJPEGQueryS {
			t.Errorf("unexpected JPEG URL: %s", request.URL.Redacted())
		}
		jpegRequests.Add(1)
		response.Header().Set("Content-Type", "image/jpeg")
		_, _ = response.Write(jpegData)
	}))
	t.Cleanup(jpegServer.Close)
	jpegPort := jpegServer.Listener.Addr().(*net.TCPAddr).Port

	outputDirectory := t.TempDir()
	cfg := DefaultConfig()
	cfg.Username = "cellphone0"
	cfg.Password = "test"
	cfg.ProxyHost = "127.0.0.1:9"
	cfg.BindHost = "127.0.0.1"
	cfg.BindPort = agentPort
	cfg.ContactHost = "127.0.0.1"
	cfg.ContactPort = agentPort
	cfg.ContactParams = ";+l.operator.docomo.intercom"
	cfg.RegisterURI = "sip:cellphone0@127.0.0.1:9"
	cfg.MessageURI = "sip:housing@127.0.0.1:9"
	cfg.EntranceURI = "sip:housing@127.0.0.1:9"
	cfg.AnswerCalls = false
	cfg.SendMessages = false
	cfg.CaptureImages = false
	cfg.AllowedCallers = []string{"interphone0", "interphone1"}
	cfg.UnlockCallers = []string{"interphone0"}
	cfg.RejectStatus = 486
	cfg.RejectReason = "Busy Here"
	cfg.IncomingJPEGDir = outputDirectory
	cfg.IncomingJPEGHold = Duration{Duration: 120 * time.Millisecond}
	cfg.IncomingJPEGInterval = Duration{Duration: 40 * time.Millisecond}

	agent, err := NewAgent(cfg, NewMessageBuffer(1))
	if err != nil {
		t.Fatal(err)
	}
	messageClient := &alwaysOKSIPClient{}
	agent.cli = messageClient
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

	client, clientAddr, cleanupClient := startUAC(t)
	t.Cleanup(cleanupClient)
	host, port, err := sip.ParseAddr(clientAddr)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		caller        string
		captureImages bool
		autoUnlock    bool
		wantJPEG      bool
		wantUnlock    bool
	}{
		{
			name:          "capture only",
			caller:        "interphone0",
			captureImages: true,
			wantJPEG:      true,
		},
		{
			name:          "capture and unlock",
			caller:        "interphone0",
			captureImages: true,
			autoUnlock:    true,
			wantJPEG:      true,
			wantUnlock:    true,
		},
		{
			name:          "capture without unlock for excluded caller",
			caller:        "interphone1",
			captureImages: true,
			autoUnlock:    true,
			wantJPEG:      true,
		},
		{
			name:       "unlock only with JPEG directory configured",
			caller:     "interphone0",
			autoUnlock: true,
			wantUnlock: true,
		},
		{
			name:       "no capture or unlock for excluded caller",
			caller:     "interphone1",
			autoUnlock: true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent.SetCaptureEnabled(test.captureImages)
			agent.SetMessageEnabled(test.autoUnlock)
			jpegRequestsBefore := jpegRequests.Load()
			unlockCallsBefore := messageClient.unlockCalls.Load()
			outputsBefore, err := filepath.Glob(filepath.Join(outputDirectory, "*_"+test.caller+".jpg"))
			if err != nil {
				t.Fatalf("output glob before INVITE: %v", err)
			}

			dialogUA := &sipgo.DialogUA{
				Client: client,
				ContactHDR: sip.ContactHeader{Address: sip.Uri{
					Scheme: "sip",
					User:   test.caller,
					Host:   host,
					Port:   port,
				}},
			}
			offer := []byte(fmt.Sprintf(
				"v=0\r\n"+
					"o=%s %d %d IN IP4 127.0.0.1\r\n"+
					"s=session\r\n"+
					"c=IN IP4 127.0.0.1\r\n"+
					"t=0 0\r\n"+
					"m=audio 21000 RTP/AVP 0\r\n"+
					"a=rtpmap:0 PCMU/8000\r\n"+
					"a=sendrecv\r\n"+
					"m=video %d HTTP jpeg\r\n"+
					"a=sendrecv\r\n",
				test.caller, index+1, index+2, jpegPort,
			))
			dialog, err := dialogUA.Invite(context.Background(),
				sip.Uri{Scheme: "sip", User: "cellphone0", Host: "127.0.0.1", Port: agentPort},
				offer,
				sip.NewHeader("From", fmt.Sprintf("\"%s\" <sip:%s@127.0.0.1>;tag=invite-%d", test.caller, test.caller, index)),
				sip.NewHeader("Content-Type", "application/sdp"),
				sip.NewHeader("Supported", "100rel"),
			)
			if err != nil {
				t.Fatalf("INVITE: %v", err)
			}

			got183 := false
			pracked := false
			answerCtx, cancelAnswer := context.WithTimeout(context.Background(), 5*time.Second)
			err = dialog.WaitAnswer(answerCtx, sipgo.AnswerOptions{OnResponse: func(response *sip.Response) error {
				if response.StatusCode != 183 {
					return nil
				}
				got183 = true
				if header := response.GetHeader("Require"); header == nil || !headerHasToken(header.Value(), "100rel") {
					return fmt.Errorf("183 has no Require: 100rel")
				}
				if header := response.GetHeader("RSeq"); header == nil || header.Value() != "1000" {
					return fmt.Errorf("183 has unexpected RSeq: %v", header)
				}
				if !strings.Contains(string(response.Body()), "m=video 8080 HTTP jpeg\r\na=recvonly\r\n") {
					return fmt.Errorf("183 has unexpected SDP: %q", response.Body())
				}
				if pracked {
					return nil
				}
				pracked = true
				contact := response.Contact()
				if contact == nil {
					return fmt.Errorf("183 has no Contact")
				}
				prack := sip.NewRequest(sip.PRACK, contact.Address)
				prack.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@127.0.0.1>;tag=changed-%d", test.caller, index)))
				prack.AppendHeader(sip.NewHeader("RAck", "(null)2 INVITE"))
				prackCtx, cancelPRACK := context.WithTimeout(context.Background(), time.Second)
				prackResponse, err := dialog.Do(prackCtx, prack)
				cancelPRACK()
				if err != nil {
					return fmt.Errorf("PRACK: %w", err)
				}
				if prackResponse.StatusCode != 200 {
					return fmt.Errorf("PRACK status = %d", prackResponse.StatusCode)
				}
				return nil
			}})
			cancelAnswer()
			if err == nil {
				t.Fatal("expected final rejection")
			}
			responseErr, ok := err.(*sipgo.ErrDialogResponse)
			if !ok || responseErr.Res.StatusCode != 486 {
				t.Fatalf("final response = %T %v", err, err)
			}
			if test.wantJPEG && (!got183 || !pracked) {
				t.Fatalf("missing early dialog: 183=%v PRACK=%v", got183, pracked)
			}
			if !test.wantJPEG && (got183 || pracked) {
				t.Fatalf("unexpected early dialog without capture: 183=%v PRACK=%v", got183, pracked)
			}

			outputs, err := filepath.Glob(filepath.Join(outputDirectory, "*_"+test.caller+".jpg"))
			if err != nil {
				t.Fatalf("output glob after INVITE: %v", err)
			}
			wantFileCount := len(outputsBefore)
			if test.wantJPEG {
				wantFileCount++
			}
			if len(outputs) != wantFileCount {
				t.Fatalf("outputs = %v, want %d timestamped files", outputs, wantFileCount)
			}
			if test.wantJPEG {
				data, err := os.ReadFile(outputs[len(outputs)-1])
				if err != nil {
					t.Fatalf("output: %v", err)
				}
				if len(data) == 0 {
					t.Fatal("output is empty")
				}
			}

			jpegRequestDelta := jpegRequests.Load() - jpegRequestsBefore
			if test.wantJPEG && jpegRequestDelta < 1 {
				t.Fatalf("JPEG request count changed by %d, want at least 1", jpegRequestDelta)
			}
			if !test.wantJPEG && jpegRequestDelta != 0 {
				t.Fatalf("JPEG request count changed by %d, want 0", jpegRequestDelta)
			}
			unlockCallDelta := messageClient.unlockCalls.Load() - unlockCallsBefore
			wantUnlockCalls := int32(0)
			if test.wantUnlock {
				wantUnlockCalls = 1
			}
			if unlockCallDelta != wantUnlockCalls {
				t.Fatalf("unlock MESSAGE count changed by %d, want %d", unlockCallDelta, wantUnlockCalls)
			}
		})
	}
}

func asDecimal(value int) string {
	return strconv.Itoa(value)
}
