// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image/jpeg"
	"sync"
)

// FrameHub keeps the latest monitor frame and fans new frames out to
// subscribers. Each subscriber has room for one frame; a newer frame replaces
// an unread older frame instead of delaying the producer.
//
// The zero value is ready for use.
type FrameHub struct {
	mu          sync.RWMutex
	latest      monitorJPEGFrame
	hasLatest   bool
	nextID      uint64
	subscribers map[uint64]chan monitorJPEGFrame
}

// FrameSubscription is a bounded stream of the latest frames from a FrameHub.
// Close is safe to call more than once.
type FrameSubscription struct {
	hub  *FrameHub
	id   uint64
	ch   <-chan monitorJPEGFrame
	done chan struct{}
	once sync.Once
}

// NewFrameHub creates an empty frame hub.
func NewFrameHub() *FrameHub {
	return &FrameHub{subscribers: make(map[uint64]chan monitorJPEGFrame)}
}

// Publish validates and retains frame, then offers it to every subscriber.
// It never waits for a subscriber to consume an older frame.
func (h *FrameHub) Publish(frame monitorJPEGFrame) error {
	owned := cloneMonitorJPEGFrame(frame)
	if err := prepareMonitorJPEGFrame(&owned); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.latest = owned
	h.hasLatest = true
	for _, subscriber := range h.subscribers {
		delivery := cloneMonitorJPEGFrame(owned)
		select {
		case subscriber <- delivery:
			continue
		default:
		}

		// The buffer contains a stale frame. A concurrent consumer might have
		// taken it since the first select, so both operations remain non-blocking.
		select {
		case <-subscriber:
		default:
		}
		select {
		case subscriber <- delivery:
		default:
		}
	}
	return nil
}

// Snapshot returns a defensive copy of the latest frame.
func (h *FrameHub) Snapshot() (monitorJPEGFrame, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.hasLatest {
		return monitorJPEGFrame{}, false
	}
	return cloneMonitorJPEGFrame(h.latest), true
}

// Subscribe creates a latest-frame subscription tied to ctx. If a frame has
// already been published, the subscription starts with a copy of that frame.
// Canceling ctx or calling Close removes the subscription and closes Frames.
func (h *FrameHub) Subscribe(ctx context.Context) *FrameSubscription {
	if ctx == nil {
		ctx = context.Background()
	}

	frames := make(chan monitorJPEGFrame, 1)
	h.mu.Lock()
	if h.subscribers == nil {
		h.subscribers = make(map[uint64]chan monitorJPEGFrame)
	}
	h.nextID++
	id := h.nextID
	h.subscribers[id] = frames
	if h.hasLatest {
		frames <- cloneMonitorJPEGFrame(h.latest)
	}
	h.mu.Unlock()

	subscription := &FrameSubscription{
		hub:  h,
		id:   id,
		ch:   frames,
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

// Frames returns the subscription's receive-only frame channel.
func (s *FrameSubscription) Frames() <-chan monitorJPEGFrame {
	if s == nil {
		return nil
	}
	return s.ch
}

// Close unsubscribes from the hub and closes Frames.
func (s *FrameSubscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.hub.unsubscribe(s.id)
		close(s.done)
	})
}

func (h *FrameHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	frames, ok := h.subscribers[id]
	if !ok {
		return
	}
	delete(h.subscribers, id)
	select {
	case <-frames:
	default:
	}
	close(frames)
}

func cloneMonitorJPEGFrame(frame monitorJPEGFrame) monitorJPEGFrame {
	frame.Data = append([]byte(nil), frame.Data...)
	return frame
}

func prepareMonitorJPEGFrame(frame *monitorJPEGFrame) error {
	if len(frame.Data) == 0 {
		return errors.New("publish monitor JPEG: empty frame")
	}
	if len(frame.Data) > monitorJPEGMaxBytes {
		return fmt.Errorf("publish monitor JPEG: frame exceeds %d bytes", monitorJPEGMaxBytes)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(frame.Data))
	if err != nil {
		return fmt.Errorf("publish monitor JPEG: decode config: %w", err)
	}
	if frame.Width != config.Width || frame.Height != config.Height {
		return fmt.Errorf(
			"publish monitor JPEG: dimensions %dx%d do not match JPEG %dx%d",
			frame.Width, frame.Height, config.Width, config.Height,
		)
	}

	digest := sha256.Sum256(frame.Data)
	if frame.SHA256 == ([sha256.Size]byte{}) {
		frame.SHA256 = digest
	} else if frame.SHA256 != digest {
		return errors.New("publish monitor JPEG: SHA-256 does not match frame data")
	}
	return nil
}
