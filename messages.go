// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi
//
// This file keeps your inbound MESSAGE structure and adds a small in-memory ring buffer
// so that a future Web API can expose recent inbound SIP MESSAGE payloads.

package main

import (
	"sync"
	"time"
)

type InboundMessage struct {
	At          time.Time `json:"at"`
	From        string    `json:"from"`
	To          string    `json:"to"`
	CallID      string    `json:"call_id"`
	ContentType string    `json:"content_type"`
	Body        []byte    `json:"body"`
}

type MessageBuffer struct {
	mu   sync.Mutex
	cap  int
	buf  []InboundMessage
	next int
	full bool
}

func NewMessageBuffer(capacity int) *MessageBuffer {
	if capacity <= 0 {
		capacity = 100
	}
	return &MessageBuffer{
		cap: capacity,
		buf: make([]InboundMessage, capacity),
	}
}

func (b *MessageBuffer) Add(m InboundMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf[b.next] = m
	b.next++
	if b.next >= b.cap {
		b.next = 0
		b.full = true
	}
}

// Snapshot returns messages in chronological order (oldest -> newest).
func (b *MessageBuffer) Snapshot() []InboundMessage {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []InboundMessage
	if !b.full {
		out = make([]InboundMessage, b.next)
		copy(out, b.buf[:b.next])
		return out
	}

	out = make([]InboundMessage, b.cap)
	// from next..end
	n := copy(out, b.buf[b.next:])
	// from 0..next-1
	copy(out[n:], b.buf[:b.next])
	return out
}
