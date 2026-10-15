// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestAudioHubPublishesDefensiveCopies(t *testing.T) {
	hub := NewAudioHub()
	subscription := hub.Subscribe(context.Background())
	defer subscription.Close()

	want := []byte{0x01, 0x02, 0x03}
	hub.Publish(want)
	want[0] = 0xff

	select {
	case got := <-subscription.Packets():
		if !bytes.Equal(got, []byte{0x01, 0x02, 0x03}) {
			t.Fatalf("packet = %x", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audio packet")
	}
}

func TestAudioHubSlowSubscriberKeepsRecentAudio(t *testing.T) {
	hub := NewAudioHub()
	subscription := hub.Subscribe(context.Background())
	defer subscription.Close()

	for i := 0; i < audioSubscriberBuffer+3; i++ {
		hub.Publish([]byte{byte(i)})
	}
	first := <-subscription.Packets()
	if first[0] == 0 {
		t.Fatal("slow subscriber retained the oldest packet")
	}
}

func TestAudioSubscriptionStopsWithContext(t *testing.T) {
	hub := NewAudioHub()
	ctx, cancel := context.WithCancel(context.Background())
	subscription := hub.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-subscription.Packets():
		if ok {
			t.Fatal("packet channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("subscription did not stop")
	}
}
