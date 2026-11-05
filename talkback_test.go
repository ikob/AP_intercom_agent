// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

type recordingTalkbackWriter struct {
	mu     sync.Mutex
	frames [][]byte
	wake   chan struct{}
}

func newRecordingTalkbackWriter() *recordingTalkbackWriter {
	return &recordingTalkbackWriter{wake: make(chan struct{}, 32)}
}

func (w *recordingTalkbackWriter) WritePCMU(frame []byte) error {
	w.mu.Lock()
	w.frames = append(w.frames, append([]byte(nil), frame...))
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

func (w *recordingTalkbackWriter) waitFor(t *testing.T, predicate func([]byte) bool) []byte {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		for _, frame := range w.frames {
			if predicate(frame) {
				copyFrame := append([]byte(nil), frame...)
				w.mu.Unlock()
				return copyFrame
			}
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-deadline.C:
			t.Fatal("timed out waiting for talkback frame")
		}
	}
}

func TestTalkbackControllerGatesMicrophoneWithPCMUAlarmSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newTalkbackController()
	controller.Run(ctx)
	writer := newRecordingTalkbackWriter()
	controller.SetTarget("monitor:test", writer)

	writer.waitFor(t, func(frame []byte) bool { return bytes.Equal(frame, talkbackSilenceFrame) })
	release, err := controller.BeginInput()
	if err != nil {
		t.Fatal(err)
	}
	voice := bytes.Repeat([]byte{0x55}, audioPCMUFrameBytes)
	controller.Publish(voice)
	got := writer.waitFor(t, func(frame []byte) bool { return bytes.Equal(frame, voice) })
	if !bytes.Equal(got, voice) {
		t.Fatalf("voice frame = %x, want %x", got[:8], voice[:8])
	}
	release()

	writer.mu.Lock()
	writer.frames = nil
	writer.mu.Unlock()
	writer.waitFor(t, func(frame []byte) bool { return bytes.Equal(frame, talkbackSilenceFrame) })
}

func TestTalkbackTargetReleaseCannotClearNewDialog(t *testing.T) {
	controller := newTalkbackController()
	releaseOld := controller.SetTarget("monitor:old", newRecordingTalkbackWriter())
	controller.SetTarget("incoming:new", newRecordingTalkbackWriter())
	releaseOld()

	snapshot := controller.Snapshot()
	if !snapshot.Available || snapshot.TargetID != "incoming:new" {
		t.Fatalf("snapshot = %+v, want new target available", snapshot)
	}
}
