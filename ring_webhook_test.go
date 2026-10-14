// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRingWebhookURL(t *testing.T) {
	for _, value := range []string{"", "http://homeassistant.local/api/webhook/secret", "https://example.com/hook"} {
		if err := validateRingWebhookURL(value); err != nil {
			t.Errorf("validateRingWebhookURL(%q): %v", value, err)
		}
	}
	for _, value := range []string{"homeassistant.local/hook", "ftp://example.com/hook", "http:///hook", "http://user:pass@example.com/hook", "http://example.com/hook#secret"} {
		if err := validateRingWebhookURL(value); err == nil {
			t.Errorf("validateRingWebhookURL(%q) unexpectedly succeeded", value)
		}
	}
}

func TestPostRingWebhookRejectsHTTPErrorWithoutLeakingURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	secret := "do-not-log-this-webhook-id"
	err := postRingWebhook(context.Background(), server.URL+"/api/webhook/"+secret, RingWebhookEvent{Type: "ring"})
	if err == nil {
		t.Fatal("expected webhook failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked webhook URL: %v", err)
	}
}

func TestNotifyRingWebhookDisabled(t *testing.T) {
	if (&Agent{}).notifyRingWebhook(context.Background(), dialogWithInvite(nil), "interphone0") {
		t.Fatal("webhook without a configured URL was scheduled")
	}
}
