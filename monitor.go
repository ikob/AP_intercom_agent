// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type monitorState uint8

const (
	monitorIdle monitorState = iota
	monitorStarting
	monitorConfirmed
	monitorStopping
)

var errMonitorActive = errors.New("monitor dialog is already active")
var errIncomingCallActive = errors.New("incoming call has priority over monitor")

const (
	monitorTalkingContentType = "application/intercom.message+xml"
	monitorTalkingBody        = "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\r\n" +
		"<IFBOX version = \"1.0\">\r\n" +
		"    <SYSTEM>\r\n" +
		"        <SYS_COMMAND>\r\n" +
		"            <USER_COMM  state = \"request\"  type = \"talking\">CONNECT</USER_COMM>\r\n" +
		"        </SYS_COMMAND>\r\n" +
		"    </SYSTEM>\r\n" +
		"</IFBOX>\r\n"
)

func (a *Agent) StartMonitor(ctx context.Context) error {
	if a.cfg.MonitorURI == "" {
		return errors.New("monitor URI is not configured")
	}
	if a.hasActiveIncomingCall() {
		return errIncomingCallActive
	}

	a.monitorMu.Lock()
	if a.monitorState != monitorIdle {
		a.monitorMu.Unlock()
		return errMonitorActive
	}
	dialCtx, cancelDial := context.WithCancel(ctx)
	a.monitorState = monitorStarting
	a.monitorStartCancel = cancelDial
	a.monitorLifecycle = make(chan struct{})
	a.monitorTalkingCallID = ""
	lifecycle := a.monitorLifecycle
	a.monitorMu.Unlock()

	dialog, err := a.dialMonitor(dialCtx)
	setupCanceled := dialCtx.Err() != nil
	cancelDial()
	incomingActive := a.hasActiveIncomingCall()

	a.monitorMu.Lock()
	a.monitorStartCancel = nil
	if err != nil {
		a.monitorState = monitorIdle
		a.closeMonitorLifecycleLocked(lifecycle)
		a.monitorMu.Unlock()
		return err
	}
	// A priority caller may have canceled the setup just as the final 200/ACK
	// completed. Do not expose that dialog as an active monitor.
	if setupCanceled || incomingActive {
		a.monitorState = monitorStopping
		a.monitorDialog = dialog
		a.monitorMu.Unlock()
		stopCtx, cancel := context.WithTimeout(context.Background(), cameraMonitorStopTimeout)
		err := a.finishStoppingMonitor(stopCtx, dialog, lifecycle)
		cancel()
		if err != nil {
			return errors.Join(errIncomingCallActive, err)
		}
		return errIncomingCallActive
	}
	a.monitorDialog = dialog
	a.monitorState = monitorConfirmed
	a.monitorMu.Unlock()
	return nil
}

func (a *Agent) StopMonitor(ctx context.Context) error {
	a.monitorMu.Lock()
	switch a.monitorState {
	case monitorIdle:
		a.monitorMu.Unlock()
		return nil
	case monitorStarting:
		cancel := a.monitorStartCancel
		done := a.monitorLifecycle
		a.monitorMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return waitMonitorLifecycle(ctx, done)
	case monitorStopping:
		done := a.monitorLifecycle
		a.monitorMu.Unlock()
		return waitMonitorLifecycle(ctx, done)
	}
	dialog := a.monitorDialog
	lifecycle := a.monitorLifecycle
	if dialog == nil {
		a.monitorState = monitorIdle
		a.closeMonitorLifecycleLocked(lifecycle)
		a.monitorMu.Unlock()
		return nil
	}
	a.monitorState = monitorStopping
	a.monitorMu.Unlock()
	return a.finishStoppingMonitor(ctx, dialog, lifecycle)
}

func (a *Agent) finishStoppingMonitor(ctx context.Context, dialog *diago.DialogClientSession, lifecycle chan struct{}) error {
	callID := monitorCallID(dialog)
	remoteEnded := dialog.Context().Err() != nil
	var byeErr error
	if !remoteEnded {
		byeErr = dialog.Hangup(ctx)
	}
	closeErr := dialog.Close()

	a.monitorMu.Lock()
	if a.monitorDialog == dialog {
		a.monitorDialog = nil
		a.monitorState = monitorIdle
		a.monitorTalkingCallID = ""
	}
	a.closeMonitorLifecycleLocked(lifecycle)
	a.monitorMu.Unlock()

	if remoteEnded {
		slog.Info("Monitor ended by remote", "call_id", callID)
	} else if byeErr == nil {
		slog.Info("Monitor BYE completed", "call_id", callID, "status", 200)
	}
	return errors.Join(byeErr, closeErr)
}

func (a *Agent) closeMonitorLifecycleLocked(lifecycle chan struct{}) {
	if lifecycle == nil || a.monitorLifecycle != lifecycle {
		return
	}
	close(lifecycle)
	a.monitorLifecycle = nil
}

func waitMonitorLifecycle(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) monitorActive() bool {
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	return a.monitorState != monitorIdle
}

func (a *Agent) monitorDone() <-chan struct{} {
	a.monitorMu.Lock()
	defer a.monitorMu.Unlock()
	if a.monitorState != monitorConfirmed || a.monitorDialog == nil {
		return nil
	}
	return a.monitorDialog.Context().Done()
}

// StartMonitorAudio asks IFBOX to begin the audio portion of the active
// monitor dialog and starts relaying received PCMU payloads. It is idempotent
// for a dialog. Sending audio back uses the same dialog's RTP writer later, so
// enabling talkback will not require replacing the receive path.
func (a *Agent) StartMonitorAudio(ctx context.Context) error {
	a.monitorMu.Lock()
	if a.monitorState != monitorConfirmed || a.monitorDialog == nil {
		a.monitorMu.Unlock()
		return errors.New("monitor dialog is not confirmed")
	}
	dialog := a.monitorDialog
	callID := monitorCallID(dialog)
	if callID != "" && a.monitorTalkingCallID == callID {
		a.monitorMu.Unlock()
		return nil
	}
	a.monitorTalkingCallID = callID
	a.monitorMu.Unlock()

	if err := a.sendMessage(ctx, a.monitorRecipient, monitorTalkingContentType, []byte(monitorTalkingBody)); err != nil {
		a.monitorMu.Lock()
		if a.monitorDialog == dialog && a.monitorTalkingCallID == callID {
			a.monitorTalkingCallID = ""
		}
		a.monitorMu.Unlock()
		return fmt.Errorf("start monitor audio: %w", err)
	}

	a.monitorMu.Lock()
	stillActive := a.monitorState == monitorConfirmed && a.monitorDialog == dialog
	a.monitorMu.Unlock()
	if !stillActive {
		return errors.New("monitor dialog ended while starting audio")
	}
	go a.relayMonitorAudio(dialog, callID)
	slog.Info("Monitor audio requested", "call_id", callID)
	return nil
}

func (a *Agent) relayMonitorAudio(dialog *diago.DialogClientSession, callID string) {
	if dialog == nil || dialog.Media() == nil || dialog.Media().RTPPacketReader == nil || a.audioHub == nil {
		slog.Warn("Monitor audio relay unavailable", "call_id", callID)
		return
	}
	reader := dialog.Media().RTPPacketReader
	buffer := make([]byte, media.RTPBufSize)
	packets := 0
	bytes := 0
	for {
		n, err := reader.Read(buffer)
		if n > 0 && reader.PacketHeader.PayloadType == media.CodecAudioUlaw.PayloadType {
			a.audioHub.Publish(buffer[:n])
			packets++
			bytes += n
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && dialog.Context().Err() == nil {
				slog.Warn("Monitor audio relay stopped", "call_id", callID, "error", err)
			} else {
				slog.Info("Monitor audio relay stopped", "call_id", callID, "packets", packets, "bytes", bytes)
			}
			return
		}
	}
}

func (a *Agent) dialMonitor(ctx context.Context) (_ *diago.DialogClientSession, retErr error) {
	dialog, err := a.tu.NewDialog(a.monitorRecipient, diago.NewDialogOptions{Transport: "udp"})
	if err != nil {
		return nil, fmt.Errorf("create monitor dialog: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = dialog.Close()
		}
	}()

	mediaSession := dialog.MediaSession()
	mediaSession.Codecs = []media.Codec{media.CodecAudioUlaw}
	offer, err := buildMonitorOffer(mediaSession.LocalSDP(), a.cfg.Username, a.cfg.ContactHost)
	if err != nil {
		return nil, err
	}
	if err := mediaSession.InitWithSDP(offer); err != nil {
		return nil, fmt.Errorf("initialize monitor SDP: %w", err)
	}
	a.configureMonitorContact(dialog)
	request := dialog.InviteRequest
	request.AppendHeader(sip.HeaderClone(&dialog.DialogClientSession.UA.ContactHDR))
	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.AppendHeader(sip.NewHeader("Supported", "100rel"))
	request.AppendHeader(sip.NewHeader("Allow", "INVITE,ACK,BYE,CANCEL,MESSAGE,NOTIFY,INFO,REFER,PRACK"))
	request.SetBody(offer)

	seenRSeq := make(map[uint32]struct{})
	if err := dialog.DialogClientSession.Invite(ctx, sipgo.ClientRequestBuild); err != nil {
		return nil, fmt.Errorf("send monitor INVITE: %w", err)
	}
	err = dialog.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: a.cfg.Username,
		Password: a.cfg.Password,
		OnResponse: func(res *sip.Response) error {
			callID := monitorCallID(dialog)
			slog.Info("Monitor INVITE response", "call_id", callID, "status", res.StatusCode, "reason", res.Reason)

			rseq, reliable, err := reliableSequence(res)
			if err != nil {
				return err
			}
			if !reliable {
				return nil
			}
			if _, ok := seenRSeq[rseq]; ok {
				return nil
			}
			if err := a.sendMonitorPRACK(ctx, dialog, res, rseq); err != nil {
				return err
			}
			seenRSeq[rseq] = struct{}{}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("monitor INVITE: %w", err)
	}

	if err := dialog.DialogClientSession.Ack(ctx); err != nil {
		return nil, fmt.Errorf("monitor ACK: %w", err)
	}
	slog.Info("Monitor dialog confirmed", "call_id", monitorCallID(dialog), "status", dialog.InviteResponse.StatusCode)
	return dialog, nil
}

func (a *Agent) sendMonitorPRACK(ctx context.Context, dialog *diago.DialogClientSession, provisional *sip.Response, rseq uint32) error {
	contact := provisional.Contact()
	if contact == nil {
		return errors.New("reliable provisional response has no Contact")
	}
	cseq := dialog.InviteRequest.CSeq()
	if cseq == nil {
		return errors.New("monitor INVITE has no CSeq")
	}

	request := sip.NewRequest(sip.PRACK, contact.Address)
	request.AppendHeader(sip.NewHeader("RAck", fmt.Sprintf("%d %d INVITE", rseq, cseq.SeqNo)))
	response, err := dialog.Do(ctx, request)
	if err != nil {
		return fmt.Errorf("monitor PRACK: %w", err)
	}
	if !response.IsSuccess() {
		return fmt.Errorf("monitor PRACK failed: %s", response.StartLine())
	}
	slog.Info("Monitor PRACK completed", "call_id", monitorCallID(dialog), "status", response.StatusCode, "rseq", rseq)
	return nil
}

func reliableSequence(res *sip.Response) (uint32, bool, error) {
	if res == nil || res.StatusCode <= 100 || res.StatusCode >= 200 {
		return 0, false, nil
	}
	require := res.GetHeader("Require")
	if require == nil || !headerHasToken(require.Value(), "100rel") {
		return 0, false, nil
	}
	rseqHeader := res.GetHeader("RSeq")
	if rseqHeader == nil {
		return 0, false, errors.New("reliable provisional response has no RSeq")
	}
	rseq, err := strconv.ParseUint(strings.TrimSpace(rseqHeader.Value()), 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("invalid RSeq %q: %w", rseqHeader.Value(), err)
	}
	return uint32(rseq), true, nil
}

func headerHasToken(value, want string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), want) {
			return true
		}
	}
	return false
}

func buildMonitorOffer(base []byte, username, advertisedHost string) ([]byte, error) {
	if username == "" {
		username = "-"
	}
	normalized := strings.ReplaceAll(string(base), "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(normalized), "\n")
	out := make([]string, 0, len(lines)+2)
	foundAudio := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "o="):
			fields := strings.Fields(line)
			if len(fields) < 6 {
				return nil, fmt.Errorf("invalid SDP origin line %q", line)
			}
			fields[0] = "o=" + username
			if advertisedHost != "" {
				fields[5] = advertisedHost
			}
			line = strings.Join(fields, " ")
		case strings.HasPrefix(line, "c="):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return nil, fmt.Errorf("invalid SDP connection line %q", line)
			}
			if advertisedHost != "" {
				fields[2] = advertisedHost
			}
			line = strings.Join(fields, " ")
		case strings.HasPrefix(line, "s="):
			line = "s=session"
		case strings.HasPrefix(line, "m=audio "):
			foundAudio = true
		case strings.HasPrefix(line, "m=video "):
			return nil, errors.New("base SDP already contains video media")
		case strings.HasPrefix(line, "a=ptime:"), strings.HasPrefix(line, "a=maxptime:"):
			continue
		}
		out = append(out, line)
	}
	if !foundAudio {
		return nil, errors.New("base SDP has no audio media")
	}
	out = append(out, "m=video 8080 HTTP jpeg", "a=recvonly")
	return []byte(strings.Join(out, "\r\n") + "\r\n"), nil
}

func (a *Agent) configureMonitorContact(dialog *diago.DialogClientSession) {
	contact := &dialog.DialogClientSession.UA.ContactHDR
	contact.DisplayName = ""
	contact.Address.Scheme = "sip"
	contact.Address.User = a.cfg.Username
	if a.cfg.ContactHost != "" {
		contact.Address.Host = a.cfg.ContactHost
	}
	if a.cfg.ContactPort != 0 {
		contact.Address.Port = a.cfg.ContactPort
	}
	contact.Address.UriParams = sip.NewParams()
	contact.Address.Headers = sip.NewParams()
	contact.Params = parseContactParams(a.cfg.ContactParams)
}

func parseContactParams(value string) sip.HeaderParams {
	params := sip.NewParams()
	for _, item := range strings.Split(strings.Trim(value, " ;"), ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 1 {
			params.Add(parts[0], "")
			continue
		}
		params.Add(parts[0], parts[1])
	}
	return params
}

func monitorCallID(dialog *diago.DialogClientSession) string {
	if dialog == nil || dialog.InviteRequest == nil || dialog.InviteRequest.CallID() == nil {
		return ""
	}
	return dialog.InviteRequest.CallID().Value()
}

func (a *Agent) RunMonitorProbe(ctx context.Context) error {
	if !a.cfg.MonitorProbe {
		return errors.New("monitor probe is not enabled")
	}

	// Keep the SIP transport alive long enough to perform a graceful BYE even
	// when the outer signal context is canceled.
	sipCtx, stopSIP := context.WithCancel(context.Background())
	defer stopSIP()
	if err := a.tu.ServeBackground(sipCtx, func(dialog *diago.DialogServerSession) {
		_ = dialog.Trying()
		_ = dialog.Respond(480, "Monitor Probe Active", nil)
	}); err != nil {
		return fmt.Errorf("start SIP transport: %w", err)
	}

	registerCtx, cancelRegister := context.WithTimeout(ctx, 5*time.Second)
	registerResponse, err := a.doRegisterOnce(registerCtx, int(a.cfg.Expiry.Duration.Seconds()))
	cancelRegister()
	if err != nil {
		return fmt.Errorf("monitor probe REGISTER: %w", err)
	}
	slog.Info("Monitor probe REGISTER completed", "status", registerResponse.StatusCode)

	defer func() {
		unregisterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := a.doRegisterOnce(unregisterCtx, 0); err != nil {
			slog.Warn("Monitor probe unregister failed", "error", err)
		}
	}()

	confirmed := 0
	byeAcknowledged := 0
	jpegFrames := 0
	for iteration := 1; iteration <= a.cfg.MonitorRepeat; iteration++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		slog.Info("Monitor probe iteration starting", "iteration", iteration, "total", a.cfg.MonitorRepeat)

		setupCtx, cancelSetup := context.WithTimeout(ctx, 10*time.Second)
		err := a.StartMonitor(setupCtx)
		cancelSetup()
		if err != nil {
			return fmt.Errorf("monitor probe iteration %d start: %w", iteration, err)
		}
		confirmed++

		var activityErr error
		if a.cfg.MonitorJPEGOut == "" {
			timer := time.NewTimer(a.cfg.MonitorHold.Duration)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		} else {
			holdCtx, cancelHold := context.WithTimeout(ctx, a.cfg.MonitorHold.Duration)
			frames, changed, err := a.pollMonitorJPEG(holdCtx, a.cfg.MonitorJPEGOut, a.cfg.MonitorJPEGInterval.Duration)
			cancelHold()
			jpegFrames += frames
			activityErr = err
			slog.Info("Monitor JPEG polling completed",
				"iteration", iteration,
				"frames", frames,
				"changed", changed,
			)
		}

		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		stopErr := a.StopMonitor(stopCtx)
		cancelStop()
		if stopErr == nil {
			byeAcknowledged++
		}
		if ctx.Err() != nil {
			activityErr = nil
		}
		if err := errors.Join(activityErr, stopErr); err != nil {
			return fmt.Errorf("monitor probe iteration %d: %w", iteration, err)
		}

		if err := ctx.Err(); err != nil {
			return nil
		}
	}

	slog.Info("Monitor probe completed",
		"attempts", a.cfg.MonitorRepeat,
		"confirmed", confirmed,
		"bye_acked", byeAcknowledged,
		"jpeg_frames", jpegFrames,
		"result", "pass",
	)
	return nil
}
