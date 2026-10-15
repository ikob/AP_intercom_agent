// SPDX-License-Identifier: Apache-2.0
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
	"github.com/pion/rtp"
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

type incomingCallMedia struct {
	answer        []byte
	audioSocket   *net.UDPConn
	cancel        context.CancelFunc
	stopDialog    func() bool
	captureCancel context.CancelFunc
	captureDone   chan struct{}
	audioDone     chan struct{}
}

func (m *incomingCallMedia) Close() {
	if m == nil {
		return
	}
	if m.captureCancel != nil {
		m.captureCancel()
	}
	if m.stopDialog != nil {
		m.stopDialog()
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.audioSocket != nil {
		_ = m.audioSocket.Close()
	}
	if m.captureDone != nil {
		<-m.captureDone
	}
	if m.audioDone != nil {
		<-m.audioDone
	}
}

// prepareIncomingCall establishes the reliable early dialog used by IFBOX and
// keeps the negotiated audio port alive for a later manual answer. JPEG polling
// runs independently so it never delays answer or hangup commands.
func (a *Agent) prepareIncomingCall(parent context.Context, dialog *diago.DialogServerSession, captureJPEG bool, afterProgress func()) (*incomingCallMedia, error) {
	request := dialog.InviteRequest
	if request == nil {
		return nil, errors.New("incoming dialog has no INVITE")
	}
	callID := sipRequestCallID(request)
	caller := "unknown"
	if from := request.From(); from != nil && from.Address.User != "" {
		caller = from.Address.User
	}

	answer, audioSocket, err := buildIncomingMonitorAnswer(a.cfg.ContactHost, a.cfg.Username)
	if err != nil {
		return nil, err
	}
	media := &incomingCallMedia{answer: answer, audioSocket: audioSocket}
	fail := func(err error) (*incomingCallMedia, error) {
		media.Close()
		return nil, err
	}

	callCtx, cancelCall := context.WithCancel(parent)
	stopDialogCancel := context.AfterFunc(dialog.Context(), cancelCall)
	media.cancel = cancelCall
	media.stopDialog = stopDialogCancel

	if captureJPEG {
		waiter, waiterErr := a.registerPRACKWaiter(callID, sipHeaderTag(request.To()))
		if waiterErr != nil {
			return fail(waiterErr)
		}
		defer a.unregisterPRACKWaiter(callID, waiter)
		if err := a.sendIncoming183(dialog, answer); err != nil {
			return fail(err)
		}
		slog.Info("Incoming reliable session progress sent", "call_id", callID, "caller", caller, "rseq", incomingRSeq)
		if afterProgress != nil {
			afterProgress()
		}
		prackCtx, cancelPRACK := context.WithTimeout(callCtx, incomingPRACKWait)
		err = a.waitIncomingPRACK(prackCtx, dialog, answer, waiter)
		cancelPRACK()
		if err != nil {
			return fail(fmt.Errorf("wait for incoming PRACK: %w", err))
		}
	} else {
		if err := dialog.Ringing(); err != nil {
			return fail(fmt.Errorf("send ringing: %w", err))
		}
		slog.Info("Ringing", "id", dialog.ID)
		if afterProgress != nil {
			afterProgress()
		}
	}

	if captureJPEG {
		endpoint, endpointErr := monitorJPEGEndpointFromSDP(request.Body(), a.cfg.JPEGQueryS)
		if endpointErr != nil {
			return fail(fmt.Errorf("parse incoming monitor SDP: %w", endpointErr))
		}
		if endpointErr = validateIncomingJPEGEndpoint(request, endpoint); endpointErr != nil {
			return fail(endpointErr)
		}
		if endpointErr = ensureIncomingJPEGDirectory(a.cfg.IncomingJPEGDir); endpointErr != nil {
			return fail(fmt.Errorf("create incoming JPEG directory: %w", endpointErr))
		}
		captureCtx, cancelCapture := context.WithCancel(callCtx)
		media.captureCancel = cancelCapture
		media.captureDone = make(chan struct{})
		output := filepath.Join(a.cfg.IncomingJPEGDir, incomingJPEGFilename(time.Now(), caller))
		a.markIncomingJPEGActive(output)
		go a.captureIncomingJPEG(captureCtx, media.captureDone, endpoint, output, callID, caller)
	}
	if a.audioHub != nil {
		media.audioDone = make(chan struct{})
		go a.relayIncomingAudio(callCtx, media.audioDone, audioSocket, callID, caller)
	}
	return media, nil
}

// relayIncomingAudio accepts RTP from any source port on the advertised UDP
// socket. IFBOX does not necessarily send from the audio port in its SDP.
func (a *Agent) relayIncomingAudio(ctx context.Context, done chan struct{}, socket *net.UDPConn, callID, caller string) {
	defer close(done)
	buffer := make([]byte, 2048)
	packets := 0
	bytes := 0
	for {
		n, _, err := socket.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				slog.Warn("Incoming audio relay stopped", "call_id", callID, "caller", caller, "error", err)
			} else {
				slog.Info("Incoming audio relay stopped", "call_id", callID, "caller", caller, "packets", packets, "bytes", bytes)
			}
			return
		}
		var packet rtp.Packet
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			slog.Debug("Ignored invalid incoming RTP packet", "call_id", callID, "error", err)
			continue
		}
		if packet.PayloadType != 0 || len(packet.Payload) == 0 {
			continue
		}
		a.audioHub.Publish(packet.Payload)
		packets++
		bytes += len(packet.Payload)
	}
}

func (a *Agent) captureIncomingJPEG(ctx context.Context, done chan struct{}, endpoint *url.URL, output, callID, caller string) {
	defer close(done)
	storeUntil := time.Now().Add(a.cfg.IncomingJPEGHold.Duration)
	storedFrame := false
	frames, changed, pollErr := pollMonitorJPEGEndpoint(ctx, endpoint, output, a.cfg.IncomingJPEGInterval.Duration, func(frame monitorJPEGFrame) error {
		if err := a.publishMonitorJPEGFrame(frame); err != nil {
			return err
		}
		if storedFrame && !time.Now().Before(storeUntil) {
			return nil
		}
		if err := writeMonitorJPEGAtomic(output, frame.Data); err != nil {
			slog.Warn("Failed to store incoming monitor JPEG frame", "output", output, "error", err)
			return nil
		}
		storedFrame = true
		return nil
	})
	removed, pruneErr := a.finishIncomingJPEGCapture(output, frames > 0)
	if pruneErr != nil {
		slog.Warn("Failed to apply incoming JPEG retention", "output", output, "error", pruneErr)
	}
	if removed > 0 {
		slog.Info("Incoming JPEG retention completed", "removed", removed, "remaining_limit", a.cfg.IncomingJPEGMaxFiles)
	}
	slog.Info("Incoming ring JPEG polling completed",
		"call_id", callID,
		"caller", caller,
		"frames", frames,
		"changed", changed,
		"output", output,
	)
	if pollErr != nil {
		slog.Warn("Incoming ring JPEG polling failed", "call_id", callID, "caller", caller, "error", pollErr)
	}
}

// captureIncomingRing retains the original synchronous helper for focused
// callers. Normal inbound call handling uses prepareIncomingCall directly.
func (a *Agent) captureIncomingRing(parent context.Context, dialog *diago.DialogServerSession, afterProgress func()) error {
	media, err := a.prepareIncomingCall(parent, dialog, true, afterProgress)
	if err != nil {
		return err
	}
	defer media.Close()
	if media.captureDone != nil {
		<-media.captureDone
		media.captureDone = nil
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
