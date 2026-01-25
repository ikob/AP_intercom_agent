// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"fmt"
	"testing"
)

func TestMessageBufferDefaultCapacity(t *testing.T) {
	buf := NewMessageBuffer(0)

	for i := 0; i < 100; i++ {
		buf.Add(InboundMessage{CallID: fmt.Sprintf("id-%d", i)})
	}

	snap := buf.Snapshot()
	if len(snap) != 100 {
		t.Fatalf("expected 100 messages, got %d", len(snap))
	}
	if snap[0].CallID != "id-0" {
		t.Fatalf("expected first id-0, got %q", snap[0].CallID)
	}
	if snap[99].CallID != "id-99" {
		t.Fatalf("expected last id-99, got %q", snap[99].CallID)
	}
}

func TestMessageBufferWrapOrder(t *testing.T) {
	buf := NewMessageBuffer(3)

	for i := 1; i <= 5; i++ {
		buf.Add(InboundMessage{CallID: fmt.Sprintf("id-%d", i)})
	}

	snap := buf.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(snap))
	}
	if snap[0].CallID != "id-3" || snap[1].CallID != "id-4" || snap[2].CallID != "id-5" {
		t.Fatalf("unexpected order: %q %q %q", snap[0].CallID, snap[1].CallID, snap[2].CallID)
	}
}
