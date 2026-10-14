// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emiago/diago"
)

const (
	ringWebhookDedupeWindow = 10 * time.Minute
	ringWebhookTimeout      = 5 * time.Second
)

// RingWebhookEvent is sent to Home Assistant for an allowed incoming SIP
// dialog. The configured URL is the authentication secret; it is deliberately
// not included in the payload or logs.
type RingWebhookEvent struct {
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Caller   string    `json:"caller,omitempty"`
	CallID   string    `json:"call_id,omitempty"`
	DialogID string    `json:"dialog_id,omitempty"`
}

var ringWebhookHTTPClient = &http.Client{
	Timeout: ringWebhookTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		// A redirect could disclose the secret webhook path to another server.
		return http.ErrUseLastResponse
	},
}

func validateRingWebhookURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ring-webhook-url must be a valid HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("ring-webhook-url scheme must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("ring-webhook-url must include a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("ring-webhook-url must not include user information")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("ring-webhook-url must not include a fragment")
	}
	return nil
}

// notifyRingWebhook schedules one non-blocking webhook for an allowed incoming
// SIP dialog. Call-ID is the primary deduplication key so SIP retransmissions
// do not create duplicate Home Assistant doorbell events.
func (a *Agent) notifyRingWebhook(ctx context.Context, inDialog *diago.DialogServerSession, caller string) bool {
	if a == nil || inDialog == nil || strings.TrimSpace(a.cfg.RingWebhookURL) == "" {
		return false
	}

	callID := sipRequestCallID(inDialog.InviteRequest)
	dialogID := inDialog.ID
	key := callID
	if key == "" {
		key = dialogID
	}

	now := time.Now()
	a.ringWebhookMu.Lock()
	if a.ringWebhookSeen == nil {
		a.ringWebhookSeen = make(map[string]time.Time)
	}
	for seenKey, seenAt := range a.ringWebhookSeen {
		if now.Sub(seenAt) >= ringWebhookDedupeWindow {
			delete(a.ringWebhookSeen, seenKey)
		}
	}
	if key != "" {
		if _, exists := a.ringWebhookSeen[key]; exists {
			a.ringWebhookMu.Unlock()
			return false
		}
		a.ringWebhookSeen[key] = now
	}
	a.ringWebhookMu.Unlock()

	event := RingWebhookEvent{
		Type:     "ring",
		At:       now,
		Caller:   caller,
		CallID:   callID,
		DialogID: dialogID,
	}
	go func() {
		if err := postRingWebhook(ctx, a.cfg.RingWebhookURL, event); err != nil {
			slog.Warn("Ring webhook failed",
				"caller", caller,
				"call_id", callID,
				"dialog_id", dialogID,
				"error", err,
			)
			return
		}
		slog.Info("Ring webhook sent",
			"caller", caller,
			"call_id", callID,
			"dialog_id", dialogID,
		)
	}()
	return true
}

func postRingWebhook(ctx context.Context, webhookURL string, event RingWebhookEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "AP_intercom_agent/1.0")

	response, err := ringWebhookHTTPClient.Do(request)
	if err != nil {
		// net/http errors can include the complete URL. Do not leak the webhook
		// ID through logs or the HTTP control plane.
		return fmt.Errorf("request failed (%T)", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	return nil
}
