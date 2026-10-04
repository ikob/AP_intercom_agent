// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi
//

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type Agent struct {
	cfg Config

	recipient         sip.Uri
	messageRecipient  sip.Uri
	entranceRecipient sip.Uri
	monitorRecipient  sip.Uri

	ua  *sipgo.UserAgent
	cli sipClient
	srv *sipgo.Server
	tu  *diago.Diago

	answerEnabled  atomic.Bool // controls Answer vs Reject
	messageEnabled atomic.Bool // controls MESSAGE sending

	// Registration loop cancellation so we can stop it before Unregister().
	regMu     sync.Mutex
	regCancel context.CancelFunc
	regDone   chan struct{}

	monitorMu          sync.Mutex
	monitorState       monitorState
	monitorDialog      *diago.DialogClientSession
	prackMu            sync.Mutex
	prackWaiters       map[string]*inboundPRACKWaiter
	incomingJPEGMu     sync.Mutex
	incomingJPEGActive map[string]int

	msgBuf         *MessageBuffer
	regID          uint64
	registerLoopFn func(context.Context) error
	sleepFn        func(context.Context, time.Duration) bool
}

type sipClient interface {
	Do(ctx context.Context, req *sip.Request, opts ...sipgo.ClientRequestOption) (*sip.Response, error)
	DoDigestAuth(ctx context.Context, req *sip.Request, res *sip.Response, auth sipgo.DigestAuth) (*sip.Response, error)
	Close() error
}

func NewAgent(cfg Config, msgBuf *MessageBuffer) (*Agent, error) {
	recipient := sip.Uri{}
	if err := sip.ParseUri(cfg.RegisterURI, &recipient); err != nil {
		return nil, fmt.Errorf("failed to parse register uri: %w", err)
	}
	messageRecipient := sip.Uri{}
	if err := sip.ParseUri(cfg.MessageURI, &messageRecipient); err != nil {
		return nil, fmt.Errorf("failed to parse message uri: %w", err)
	}
	entranceRecipient := sip.Uri{}
	if err := sip.ParseUri(cfg.EntranceURI, &entranceRecipient); err != nil {
		return nil, fmt.Errorf("failed to parse entrance uri: %w", err)
	}
	monitorRecipient := sip.Uri{}
	if cfg.MonitorURI != "" {
		if err := sip.ParseUri(cfg.MonitorURI, &monitorRecipient); err != nil {
			return nil, fmt.Errorf("failed to parse monitor uri: %w", err)
		}
	}

	setupLogger(cfg.LogLevel)

	useragent := cfg.Username
	if useragent == "" {
		useragent = "change-me"
	}

	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent(useragent),
		sipgo.WithUserAgentHostname(cfg.UserAgentHostname),
	)

	cli, _ := sipgo.NewClient(ua)
	srv, _ := sipgo.NewServer(ua)

	a := &Agent{
		cfg:                cfg,
		recipient:          recipient,
		messageRecipient:   messageRecipient,
		entranceRecipient:  entranceRecipient,
		monitorRecipient:   monitorRecipient,
		ua:                 ua,
		cli:                cli,
		srv:                srv,
		regDone:            make(chan struct{}),
		prackWaiters:       make(map[string]*inboundPRACKWaiter),
		incomingJPEGActive: make(map[string]int),
		msgBuf:             msgBuf,
	}
	a.srv.OnPrack(a.handleInboundPRACK)
	a.answerEnabled.Store(cfg.AnswerCalls)
	a.messageEnabled.Store(cfg.SendMessages)
	a.registerLoopFn = a.registerLoop
	a.sleepFn = sleepOrDone

	// Buffer inbound MESSAGE and always return 200 OK.
	a.srv.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)

		body := append([]byte(nil), req.Body()...)

		ct := ""
		if h := req.GetHeader("Content-Type"); h != nil {
			ct = h.Value()
		}

		im := InboundMessage{
			At:          time.Now(),
			From:        req.From().Address.String(),
			To:          req.To().Address.String(),
			CallID:      req.CallID().Value(),
			ContentType: ct,
			Body:        body,
		}

		a.msgBuf.Add(im)
		slog.Info("MESSAGE received",
			"callid", im.CallID,
			"from", im.From,
			"to", im.To,
			"content-type", im.ContentType,
			"len", len(im.Body),
		)
	})

	a.tu = diago.NewDiago(ua,
		diago.WithServer(srv),
		diago.WithTransport(
			diago.Transport{
				Transport: "udp",
				BindHost:  cfg.BindHost,
				BindPort:  cfg.BindPort,
			},
		),
	)

	return a, nil
}

func setupLogger(level string) {
	if level == "" {
		level = os.Getenv("LOG_LEVEL")
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	slog.SetLogLoggerLevel(lvl)

	media.RTPDebug = os.Getenv("RTP_DEBUG") == "true"
	media.RTCPDebug = os.Getenv("RTCP_DEBUG") == "true"
	sip.SIPDebug = os.Getenv("SIP_DEBUG") == "true"
	sip.TransactionFSMDebug = os.Getenv("SIP_TRANSACTION_DEBUG") == "true"
}

func (a *Agent) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = a.StopMonitor(ctx)
	cancel()

	// Stop register loop if running (best-effort).
	a.stopRegisterLoop()

	_ = a.cli.Close()
	_ = a.srv.Close()
	_ = a.ua.Close()
}

func (a *Agent) regOpts() diago.RegisterOptions {
	return diago.RegisterOptions{
		Username:      a.cfg.Username,
		Password:      a.cfg.Password,
		ProxyHost:     a.cfg.ProxyHost,
		Expiry:        a.cfg.Expiry.Duration,
		RetryInterval: a.cfg.RetryInterval.Duration,
	}
}

func (a *Agent) Run(ctx context.Context) error {
	if a.cfg.IncomingJPEGDir != "" {
		removed, err := a.pruneIncomingJPEGsAtStartup()
		if err != nil {
			slog.Warn("Failed to apply incoming JPEG retention at startup", "error", err)
		}
		if removed > 0 {
			slog.Info("Incoming JPEG retention completed", "removed", removed, "remaining_limit", a.cfg.IncomingJPEGMaxFiles)
		}
	}

	// Start INVITE handling.
	go func() {
		a.tu.Serve(ctx, func(inDialog *diago.DialogServerSession) {
			slog.Info("New dialog request", "id", inDialog.ID)
			defer slog.Info("Dialog finished", "id", inDialog.ID)

			// Always enforce whitelist first.
			if !a.callerAllowedFromInvite(inDialog) {
				_ = inDialog.Trying()
				_ = inDialog.Respond(403, "Forbidden", nil)
				return
			}
			caller := callerFromInvite(inDialog)

			// Snapshot the automatic-unlock state once for this call. The same
			// mode controls image capture and unlock eligibility, even if Home
			// Assistant changes the runtime state mid-call.
			autoUnlockMode := a.messageEnabled.Load()
			captureIncomingJPEG := a.shouldCaptureIncomingJPEG(autoUnlockMode)
			unlockCaller := a.unlockCallerAllowed(caller)
			if autoUnlockMode && !unlockCaller {
				slog.Info("Automatic unlock skipped for caller", "caller", caller)
			}
			if captureIncomingJPEG {
				_ = inDialog.Trying()
				slog.Info("Trying", "id", inDialog.ID)
			}

			var unlockOnce sync.Once
			sendUnlock := func() {
				if !autoUnlockMode || !unlockCaller {
					return
				}
				unlockOnce.Do(func() {
					if err := a.sendMessage(ctx, a.entranceRecipient, a.cfg.EntranceContentType, []byte(a.cfg.EntranceBody)); err != nil {
						slog.Error("Failed to send entrance MESSAGE", "error", err)
					}
				})
			}

			if captureIncomingJPEG {
				// Put the reliable 183 on the wire before unlocking, then send the
				// MESSAGE immediately; JPEG polling does not delay the unlock.
				if err := a.captureIncomingRing(ctx, inDialog, sendUnlock); err != nil && inDialog.Context().Err() == nil {
					slog.Error("Failed to capture incoming ring JPEG", "id", inDialog.ID, "error", err)
				}
				// Capture setup can fail before the 183 callback. Automatic unlock
				// must still run in that case.
				sendUnlock()
				if inDialog.Context().Err() != nil {
					return
				}
			} else {
				sendUnlock()
			}

			// If we do not answer calls, reject INVITE but keep registration alive.
			if !a.answerEnabled.Load() {
				if !captureIncomingJPEG {
					_ = inDialog.Trying()
				}
				_ = inDialog.Respond(a.cfg.RejectStatus, a.cfg.RejectReason, nil)
				return
			}

			// Normal mode: answer call and hang up shortly after.
			if err := a.respondIncomingCall(ctx, inDialog); err != nil {
				slog.Error("Failed to respond to incoming call", "error", err)
			}
		})
	}()

	if a.messageEnabled.Load() {
		if err := a.sendMessage(ctx, a.messageRecipient, a.cfg.MessageContentType, []byte(a.cfg.MessageBody)); err != nil {
			slog.Error("Failed to send initial MESSAGE", "error", err)
		}
	}

	// Always register (your current requirement).
	a.startRegisterLoop(ctx)

	<-ctx.Done()
	return nil
}

func (a *Agent) respondIncomingCall(ctx context.Context, inDialog *diago.DialogServerSession) error {
	return a.respondIncomingCallWith(ctx, inDialog.ID, inDialog)
}

type dialogResponder interface {
	Trying() error
	Ringing() error
	Answer() error
	Hangup(ctx context.Context) error
}

func (a *Agent) respondIncomingCallWith(ctx context.Context, id string, dialog dialogResponder) error {
	_ = dialog.Trying()
	slog.Info("Trying", "id", id)

	_ = dialog.Ringing()
	slog.Info("Ringing", "id", id)

	_ = dialog.Answer()
	slog.Info("Answered", "id", id)

	time.Sleep(1 * time.Second)
	if err := dialog.Hangup(ctx); err != nil {
		slog.Debug("Hangup failed", "id", id, "error", err)
	} else {
		slog.Debug("Hangup sent", "id", id)
	}
	return nil
}

func (a *Agent) callerAllowedFromInvite(inDialog *diago.DialogServerSession) bool {
	if len(a.cfg.AllowedCallers) == 0 {
		return true // Whitelist is empty, allow all
	}
	return slices.Contains(a.cfg.AllowedCallers, callerFromInvite(inDialog))
}

func callerFromInvite(inDialog *diago.DialogServerSession) string {
	if inDialog == nil {
		return ""
	}
	req := inDialog.InviteRequest
	if req == nil {
		return ""
	}

	from := req.From()
	if from == nil {
		return ""
	}
	return from.Address.User
}

// SetEnabled toggles a runtime boolean (pointer required).
func (a *Agent) SetEnabled(target *atomic.Bool, enabled bool) {
	_ = target.Swap(enabled)
}

func (a *Agent) SetAnswerEnabled(enabled bool)  { a.SetEnabled(&a.answerEnabled, enabled) }
func (a *Agent) SetMessageEnabled(enabled bool) { a.SetEnabled(&a.messageEnabled, enabled) }

func (a *Agent) shouldCaptureIncomingJPEG(autoUnlock bool) bool {
	return autoUnlock && a.cfg.IncomingJPEGDir != ""
}

func (a *Agent) unlockCallerAllowed(caller string) bool {
	return caller != "" && slices.Contains(a.cfg.UnlockCallers, caller)
}

func (a *Agent) startRegisterLoop(appCtx context.Context) {
	a.regMu.Lock()
	defer a.regMu.Unlock()

	// Don't start if already running
	if a.regCancel != nil {
		return
	}

	regCtx, cancel := context.WithCancel(appCtx)
	a.regCancel = cancel
	a.regDone = make(chan struct{})

	a.regID++
	myID := a.regID

	slog.Info("REGISTER loop starting", "id", myID)

	go func() {
		defer func() {
			close(a.regDone)
			a.regMu.Lock()
			if a.regID == myID {
				a.regCancel = nil
			}
			a.regMu.Unlock()
		}()

		if err := a.registerLoopFn(regCtx); err != nil {
			if regCtx.Err() != nil {
				slog.Info("REGISTER loop stopped (canceled)")
				return
			}
			slog.Error("REGISTER loop stopped with error", "error", err)
			return
		}
	}()
}

func (a *Agent) stopRegisterLoop() {
	a.regMu.Lock()
	cancel := a.regCancel
	done := a.regDone
	a.regID++
	a.regCancel = nil
	a.regMu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

func (a *Agent) tryUnregister(ctx context.Context) {
	uctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	rt, err := a.tu.RegisterTransaction(uctx, a.recipient, a.regOpts())
	if err != nil {
		slog.Error("failed to create register transaction for unregister", "error", err)
		return
	}
	if err := rt.Unregister(uctx); err != nil {
		slog.Error("Unregister failed", "error", err)
		return
	}
	slog.Info("Unregistered successfully")
}

func (a *Agent) contactHeaderValue() string {
	return fmt.Sprintf("<sip:%s@%s:%d>%s", a.cfg.Username, a.cfg.ContactHost, a.cfg.ContactPort, a.cfg.ContactParams)
}

func (a *Agent) sendMessage(ctx context.Context, target sip.Uri, ctype string, body []byte) error {
	req := sip.NewRequest(sip.MESSAGE, target)

	req.AppendHeader(sip.NewHeader("Contact", a.contactHeaderValue()))
	req.AppendHeader(sip.NewHeader("Content-Type", ctype))
	req.SetBody(body)

	res, err := a.cli.Do(ctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return err
	}

	if res.StatusCode == 401 || res.StatusCode == 407 {
		slog.Debug("MESSAGE auth required", "status", res.StatusCode, "target", target.String())
		auth := sipgo.DigestAuth{Username: a.cfg.Username, Password: a.cfg.Password}
		res, err = a.cli.DoDigestAuth(ctx, req, res, auth)
		if err != nil {
			return err
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		slog.Warn("MESSAGE failed", "status", res.StatusCode, "reason", res.Reason, "target", target.String())
		return fmt.Errorf("MESSAGE failed: %d %s", res.StatusCode, res.Reason)
	}
	slog.Debug("MESSAGE sent", "target", target.String(), "content_type", ctype, "body_len", len(body))
	return nil
}

func (a *Agent) registerLoop(ctx context.Context) error {
	baseExpiry := a.cfg.Expiry.Duration
	if baseExpiry <= 0 {
		baseExpiry = 3600 * time.Second
	}

	retry := a.cfg.RetryInterval.Duration
	if retry <= 0 {
		retry = 60 * time.Second
	}

	// Run immediately once, then follow the refresh cadence.
	expiry := baseExpiry
	for {
		// 1) REGISTER with expires=expiry (no auth first).
		res, err := a.doRegisterOnce(ctx, int(expiry.Seconds()))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("REGISTER attempt failed", "error", err)
			// Wait before retry.
			if !a.sleepFn(ctx, retry) {
				return ctx.Err()
			}
			continue
		}

		if res != nil {
			if srvExp, ok := responseExpirySeconds(res); ok {
				slog.Debug("REGISTER expires from response", "server_expires", srvExp, "base_expires", int(baseExpiry.Seconds()))
				expiry = time.Duration(srvExp) * time.Second
			} else {
				slog.Debug("REGISTER expires not provided; using base", "base_expires", int(baseExpiry.Seconds()))
				expiry = baseExpiry
			}
		}

		// 2) Wait until the next refresh window.
		refreshBefore := 60 * time.Second
		if expiry/3 < refreshBefore {
			refreshBefore = expiry / 3
		}
		if refreshBefore < 5*time.Second {
			refreshBefore = 5 * time.Second
		}
		wait := expiry - refreshBefore
		if wait < 1*time.Second {
			wait = 1 * time.Second
		}
		if !a.sleepFn(ctx, wait) {
			return ctx.Err()
		}

		// loop
		_ = res
	}
}

func (a *Agent) doRegisterOnce(ctx context.Context, expiresSec int) (*sip.Response, error) {
	// IMPORTANT: request is ALWAYS newly created (do not reuse).
	req := sip.NewRequest(sip.REGISTER, a.recipient)

	// Contact / Expires
	req.AppendHeader(sip.NewHeader("Contact", a.contactHeaderValue()))
	req.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", expiresSec)))

	// Send without auth first.
	res, err := a.cli.Do(ctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return nil, err
	}

	// If 401/407, resend with Digest auth.
	if res.StatusCode == 401 || res.StatusCode == 407 {
		auth := sipgo.DigestAuth{Username: a.cfg.Username, Password: a.cfg.Password}
		res, err = a.cli.DoDigestAuth(ctx, req, res, auth)
		if err != nil {
			return nil, err
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res, fmt.Errorf("REGISTER failed: %d %s", res.StatusCode, res.Reason)
	}

	slog.Debug("REGISTER ok", "expires", expiresSec)
	return res, nil
}

func responseExpirySeconds(res *sip.Response) (int, bool) {
	secs := []int{}

	if h := res.GetHeader("Expires"); h != nil {
		if v, ok := parseExpiryValue(h.Value()); ok {
			secs = append(secs, v)
		}
	}

	if c := res.Contact(); c != nil && c.Params != nil {
		if v, ok := parseExpiryParam(c.Params.Get("expires")); ok {
			secs = append(secs, v)
		}
	}

	if len(secs) == 0 {
		return 0, false
	}

	min := secs[0]
	for _, v := range secs[1:] {
		if v < min {
			min = v
		}
	}
	return min, true
}

func parseExpiryValue(value string) (int, bool) {
	secs, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || secs <= 0 {
		return 0, false
	}
	return secs, true
}

func parseExpiryParam(value string, ok bool) (int, bool) {
	if !ok {
		return 0, false
	}
	return parseExpiryValue(value)
}

// helper: sleep that can be cancelled by ctx
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
