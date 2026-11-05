// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const talkbackFrameBuffer = 8

var talkbackSilenceFrame = func() []byte {
	frame := make([]byte, audioPCMUFrameBytes)
	for i := range frame {
		frame[i] = audioPCMUSilence
	}
	return frame
}()

type talkbackFrameWriter interface {
	WritePCMU([]byte) error
}

type talkbackTarget struct {
	id         string
	generation uint64
	writer     talkbackFrameWriter
}

type talkbackSnapshot struct {
	TargetID    string `json:"target_id,omitempty"`
	Available   bool   `json:"available"`
	InputActive bool   `json:"input_active"`
	Packets     uint64 `json:"packets"`
	Bytes       uint64 `json:"bytes"`
	Dropped     uint64 `json:"dropped"`
}

// talkbackController serializes microphone PCMU frames onto the single active
// SIP media target. Once talking mode is active it keeps RTP continuous with
// PCMU silence while the microphone input is gated off, matching the official
// application's PTT behavior.
type talkbackController struct {
	mu         sync.Mutex
	target     *talkbackTarget
	generation uint64
	inputs     uint32
	frames     chan []byte
	packets    uint64
	bytes      uint64
	dropped    uint64
	runOnce    sync.Once
}

func newTalkbackController() *talkbackController {
	return &talkbackController{frames: make(chan []byte, talkbackFrameBuffer)}
}

func (c *talkbackController) Run(ctx context.Context) {
	if c == nil {
		return
	}
	c.runOnce.Do(func() {
		go c.run(ctx)
	})
}

func (c *talkbackController) run(ctx context.Context) {
	ticker := time.NewTicker(audioPCMUFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		var latest []byte
	drain:
		for {
			select {
			case latest = <-c.frames:
			default:
				break drain
			}
		}

		c.mu.Lock()
		target := c.target
		input := c.inputs > 0
		c.mu.Unlock()
		if target == nil {
			continue
		}
		frame := talkbackSilenceFrame
		if input && len(latest) == audioPCMUFrameBytes {
			frame = latest
		}
		if err := target.writer.WritePCMU(frame); err != nil {
			c.mu.Lock()
			if c.target != nil && c.target.generation == target.generation {
				c.target = nil
			}
			c.mu.Unlock()
			slog.Warn("Talkback RTP stopped", "target", target.id, "error", err)
			continue
		}
		c.mu.Lock()
		c.packets++
		c.bytes += uint64(len(frame))
		c.mu.Unlock()
	}
}

// SetTarget makes writer the exclusive active SIP talkback destination. The
// returned release function only clears the generation it created, so an old
// dialog cannot tear down a newer target.
func (c *talkbackController) SetTarget(id string, writer talkbackFrameWriter) func() {
	if c == nil || writer == nil {
		return func() {}
	}
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.target = &talkbackTarget{id: id, generation: generation, writer: writer}
	c.mu.Unlock()
	c.drainFrames()
	slog.Info("Talkback RTP target ready", "target", id)
	return func() {
		c.mu.Lock()
		if c.target != nil && c.target.generation == generation {
			c.target = nil
		}
		c.mu.Unlock()
	}
}

func (c *talkbackController) drainFrames() {
	for {
		select {
		case <-c.frames:
		default:
			return
		}
	}
}

func (c *talkbackController) BeginInput() (func(), error) {
	if c == nil {
		return nil, errors.New("talkback is unavailable")
	}
	c.mu.Lock()
	c.inputs++
	c.mu.Unlock()
	released := false
	return func() {
		c.mu.Lock()
		if released {
			c.mu.Unlock()
			return
		}
		released = true
		if c.inputs > 0 {
			c.inputs--
		}
		inactive := c.inputs == 0
		c.mu.Unlock()
		if inactive {
			c.drainFrames()
		}
	}, nil
}

func (c *talkbackController) Publish(frame []byte) {
	if c == nil || len(frame) != audioPCMUFrameBytes {
		return
	}
	copyFrame := append([]byte(nil), frame...)
	select {
	case c.frames <- copyFrame:
		return
	default:
	}
	select {
	case <-c.frames:
	default:
	}
	c.mu.Lock()
	c.dropped++
	c.mu.Unlock()
	select {
	case c.frames <- copyFrame:
	default:
	}
}

func (c *talkbackController) Snapshot() talkbackSnapshot {
	if c == nil {
		return talkbackSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := talkbackSnapshot{
		InputActive: c.inputs > 0,
		Packets:     c.packets,
		Bytes:       c.bytes,
		Dropped:     c.dropped,
	}
	if c.target != nil {
		snapshot.TargetID = c.target.id
		snapshot.Available = true
	}
	return snapshot
}
