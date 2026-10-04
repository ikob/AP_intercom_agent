// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"
)

const (
	incomingRSeq        = 1000
	incomingPRACKWait   = 4 * time.Second
	incoming183Interval = 500 * time.Millisecond
	incomingSIPAgent    = "DoCoMo/1.0 iPhone18,1(intercom)"
	incomingJPEGTimeFmt = "20060102T150405.000000000-0700"
)

type inboundPRACKWaiter struct {
	toTag string
	done  chan struct{}
	once  sync.Once
}

func (a *Agent) handleInboundPRACK(request *sip.Request, tx sip.ServerTransaction) {
	response := sip.NewResponseFromRequest(request, http.StatusOK, "OK", nil)
	response.AppendHeader(sip.NewHeader("User-Agent", incomingSIPAgent))
	if err := tx.Respond(response); err != nil {
		slog.Error("Failed to respond to PRACK", "error", err)
		return
	}

	callID := sipRequestCallID(request)
	toTag := sipHeaderTag(request.To())
	a.prackMu.Lock()
	waiter := a.prackWaiters[callID]
	if waiter != nil && (waiter.toTag == "" || waiter.toTag == toTag) {
		waiter.once.Do(func() { close(waiter.done) })
	}
	a.prackMu.Unlock()

	rack := ""
	if header := request.GetHeader("RAck"); header != nil {
		rack = header.Value()
	}
	slog.Info("Incoming PRACK completed", "call_id", callID, "status", 200, "rack", rack)
}

func (a *Agent) registerPRACKWaiter(callID, toTag string) (*inboundPRACKWaiter, error) {
	if callID == "" {
		return nil, errors.New("incoming INVITE has no Call-ID")
	}
	waiter := &inboundPRACKWaiter{toTag: toTag, done: make(chan struct{})}
	a.prackMu.Lock()
	defer a.prackMu.Unlock()
	if _, exists := a.prackWaiters[callID]; exists {
		return nil, fmt.Errorf("PRACK waiter already exists for Call-ID %s", callID)
	}
	a.prackWaiters[callID] = waiter
	return waiter, nil
}

func (a *Agent) unregisterPRACKWaiter(callID string, waiter *inboundPRACKWaiter) {
	a.prackMu.Lock()
	if a.prackWaiters[callID] == waiter {
		delete(a.prackWaiters, callID)
	}
	a.prackMu.Unlock()
}

func (a *Agent) captureIncomingRing(parent context.Context, dialog *diago.DialogServerSession, afterProgress func()) error {
	request := dialog.InviteRequest
	if request == nil {
		return errors.New("incoming dialog has no INVITE")
	}
	callID := sipRequestCallID(request)
	caller := "unknown"
	if from := request.From(); from != nil && from.Address.User != "" {
		caller = from.Address.User
	}

	endpoint, err := monitorJPEGEndpointFromSDP(request.Body(), a.cfg.JPEGQueryS)
	if err != nil {
		return fmt.Errorf("parse incoming monitor SDP: %w", err)
	}
	if err := validateIncomingJPEGEndpoint(request, endpoint); err != nil {
		return err
	}
	if err := ensureIncomingJPEGDirectory(a.cfg.IncomingJPEGDir); err != nil {
		return fmt.Errorf("create incoming JPEG directory: %w", err)
	}
	output := filepath.Join(a.cfg.IncomingJPEGDir, incomingJPEGFilename(time.Now(), caller))
	a.markIncomingJPEGActive(output)
	pruneAfterCapture := false
	defer func() {
		removed, pruneErr := a.finishIncomingJPEGCapture(output, pruneAfterCapture)
		if pruneErr != nil {
			slog.Warn("Failed to apply incoming JPEG retention", "output", output, "error", pruneErr)
		}
		if removed > 0 {
			slog.Info("Incoming JPEG retention completed", "removed", removed, "remaining_limit", a.cfg.IncomingJPEGMaxFiles)
		}
	}()

	answer, audioSocket, err := buildIncomingMonitorAnswer(a.cfg.ContactHost, a.cfg.Username)
	if err != nil {
		return err
	}
	defer audioSocket.Close()

	waiter, err := a.registerPRACKWaiter(callID, sipHeaderTag(request.To()))
	if err != nil {
		return err
	}
	defer a.unregisterPRACKWaiter(callID, waiter)

	callCtx, cancelCall := context.WithCancel(parent)
	stopDialogCancel := context.AfterFunc(dialog.Context(), cancelCall)
	defer func() {
		stopDialogCancel()
		cancelCall()
	}()

	if err := a.sendIncoming183(dialog, answer); err != nil {
		return err
	}
	slog.Info("Incoming reliable session progress sent", "call_id", callID, "caller", caller, "rseq", incomingRSeq)
	if afterProgress != nil {
		afterProgress()
	}

	prackCtx, cancelPRACK := context.WithTimeout(callCtx, incomingPRACKWait)
	err = a.waitIncomingPRACK(prackCtx, dialog, answer, waiter)
	cancelPRACK()
	if err != nil {
		return fmt.Errorf("wait for incoming PRACK: %w", err)
	}

	pollCtx, cancelPoll := context.WithTimeout(callCtx, a.cfg.IncomingJPEGHold.Duration)
	frames, changed, pollErr := pollMonitorJPEGEndpoint(pollCtx, endpoint, output, a.cfg.IncomingJPEGInterval.Duration)
	cancelPoll()
	pruneAfterCapture = frames > 0
	slog.Info("Incoming ring JPEG polling completed",
		"call_id", callID,
		"caller", caller,
		"frames", frames,
		"changed", changed,
		"output", output,
	)
	if pollErr != nil {
		return fmt.Errorf("poll incoming ring JPEG: %w", pollErr)
	}
	return nil
}

func (a *Agent) waitIncomingPRACK(ctx context.Context, dialog *diago.DialogServerSession, answer []byte, waiter *inboundPRACKWaiter) error {
	timer := time.NewTimer(incoming183Interval)
	defer timer.Stop()
	for {
		select {
		case <-waiter.done:
			return nil
		case <-timer.C:
			if err := a.sendIncoming183(dialog, answer); err != nil {
				return err
			}
			timer.Reset(incoming183Interval)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *Agent) sendIncoming183(dialog *diago.DialogServerSession, answer []byte) error {
	headers := []sip.Header{
		sip.NewHeader("Contact", a.contactHeaderValue()),
		sip.NewHeader("User-Agent", incomingSIPAgent),
		sip.NewHeader("Require", "100rel"),
		sip.NewHeader("RSeq", fmt.Sprintf("%d", incomingRSeq)),
		sip.NewHeader("Content-Type", "application/sdp"),
	}
	if err := dialog.DialogServerSession.Respond(183, "Session Progress", answer, headers...); err != nil {
		return fmt.Errorf("send reliable 183: %w", err)
	}
	return nil
}

func buildIncomingMonitorAnswer(contactHost, username string) ([]byte, *net.UDPConn, error) {
	ip := net.ParseIP(contactHost)
	if ip == nil || ip.IsUnspecified() {
		return nil, nil, fmt.Errorf("invalid contact host for incoming monitor SDP: %q", contactHost)
	}
	network := "udp4"
	addressType := "IP4"
	if ip.To4() == nil {
		network = "udp6"
		addressType = "IP6"
	}
	audioSocket, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: 0})
	if err != nil {
		return nil, nil, fmt.Errorf("allocate incoming monitor audio port: %w", err)
	}
	if username == "" {
		username = "-"
	}
	sessionID := time.Now().UnixMicro()
	port := audioSocket.LocalAddr().(*net.UDPAddr).Port
	body := fmt.Sprintf(
		"v=0\r\n"+
			"o=%s %d %d IN %s %s\r\n"+
			"s=session\r\n"+
			"c=IN %s %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP 0\r\n"+
			"a=rtpmap:0 PCMU/8000\r\n"+
			"a=sendrecv\r\n"+
			"m=video 8080 HTTP jpeg\r\n"+
			"a=recvonly\r\n",
		username, sessionID, sessionID+1, addressType, contactHost,
		addressType, contactHost, port,
	)
	return []byte(body), audioSocket, nil
}

func validateIncomingJPEGEndpoint(request *sip.Request, endpoint *url.URL) error {
	targetIP := net.ParseIP(endpoint.Hostname())
	if targetIP == nil {
		return fmt.Errorf("incoming JPEG endpoint is not an IP address: %q", endpoint.Hostname())
	}
	sourceHost, _, err := net.SplitHostPort(request.Source())
	if err != nil {
		return fmt.Errorf("parse incoming SIP source %q: %w", request.Source(), err)
	}
	sourceIP := net.ParseIP(sourceHost)
	if sourceIP == nil || !sourceIP.Equal(targetIP) {
		return fmt.Errorf("incoming JPEG endpoint %s does not match SIP source %s", targetIP, sourceHost)
	}
	return nil
}

func safeIncomingCaller(caller string) string {
	var output strings.Builder
	for _, char := range caller {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
			output.WriteRune(char)
		default:
			output.WriteByte('_')
		}
	}
	name := strings.Trim(output.String(), "_")
	if name == "" {
		return "unknown"
	}
	return name
}

func incomingJPEGFilename(at time.Time, caller string) string {
	return at.Format(incomingJPEGTimeFmt) + "_" + safeIncomingCaller(caller) + ".jpg"
}

func sipRequestCallID(request *sip.Request) string {
	if request == nil || request.CallID() == nil {
		return ""
	}
	return request.CallID().Value()
}

func sipHeaderTag(header *sip.ToHeader) string {
	if header == nil {
		return ""
	}
	tag, _ := header.Params.Get("tag")
	return tag
}
