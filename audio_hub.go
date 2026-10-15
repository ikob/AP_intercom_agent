// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"sync"
)

const audioSubscriberBuffer = 8

// AudioHub fans received G.711 PCMU payloads out to low-latency consumers.
// It intentionally keeps no snapshot: replaying old audio when a listener
// connects would add latency and produce misleading sound.
//
// The zero value is ready for use.
type AudioHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan []byte
}

type AudioSubscription struct {
	hub  *AudioHub
	id   uint64
	ch   <-chan []byte
	done chan struct{}
	once sync.Once
}

func NewAudioHub() *AudioHub {
	return &AudioHub{subscribers: make(map[uint64]chan []byte)}
}

// Publish offers a defensive copy of payload to each subscriber without
// blocking the RTP reader. When a subscriber falls behind, its oldest packet
// is discarded so live audio remains live.
func (h *AudioHub) Publish(payload []byte) {
	if h == nil || len(payload) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subscriber := range h.subscribers {
		packet := append([]byte(nil), payload...)
		select {
		case subscriber <- packet:
			continue
		default:
		}
		select {
		case <-subscriber:
		default:
		}
		select {
		case subscriber <- packet:
		default:
		}
	}
}

func (h *AudioHub) Subscribe(ctx context.Context) *AudioSubscription {
	if ctx == nil {
		ctx = context.Background()
	}
	packets := make(chan []byte, audioSubscriberBuffer)
	h.mu.Lock()
	if h.subscribers == nil {
		h.subscribers = make(map[uint64]chan []byte)
	}
	h.nextID++
	id := h.nextID
	h.subscribers[id] = packets
	h.mu.Unlock()

	subscription := &AudioSubscription{
		hub:  h,
		id:   id,
		ch:   packets,
		done: make(chan struct{}),
	}
	if ctx.Err() != nil {
		subscription.Close()
		return subscription
	}
	go func() {
		select {
		case <-ctx.Done():
			subscription.Close()
		case <-subscription.done:
		}
	}()
	return subscription
}

func (s *AudioSubscription) Packets() <-chan []byte {
	if s == nil {
		return nil
	}
	return s.ch
}

func (s *AudioSubscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.hub.unsubscribe(s.id)
		close(s.done)
	})
}

func (h *AudioHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	packets, ok := h.subscribers[id]
	if !ok {
		return
	}
	delete(h.subscribers, id)
	close(packets)
}
