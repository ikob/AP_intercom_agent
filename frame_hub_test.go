// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image/color"
	"sync"
	"testing"
	"time"
)

func testHubFrame(t testing.TB, fill color.Color) monitorJPEGFrame {
	t.Helper()
	data := testJPEG(t, fill)
	return monitorJPEGFrame{
		Data:   data,
		Width:  8,
		Height: 6,
		SHA256: sha256.Sum256(data),
	}
}

func TestFrameHubSnapshotAndDefensiveCopies(t *testing.T) {
	hub := NewFrameHub()
	if _, ok := hub.Snapshot(); ok {
		t.Fatal("empty hub unexpectedly has a snapshot")
	}

	frame := testHubFrame(t, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	original := append([]byte(nil), frame.Data...)
	frame.SHA256 = [sha256.Size]byte{} // A missing digest is filled on publish.
	if err := hub.Publish(frame); err != nil {
		t.Fatal(err)
	}
	frame.Data[0] ^= 0xff

	first, ok := hub.Snapshot()
	if !ok {
		t.Fatal("published frame is missing")
	}
	if !bytes.Equal(first.Data, original) {
		t.Fatal("snapshot aliases the publisher's data")
	}
	if first.SHA256 != sha256.Sum256(original) {
		t.Fatal("publish did not fill the missing SHA-256")
	}

	first.Data[0] ^= 0xff
	second, ok := hub.Snapshot()
	if !ok || !bytes.Equal(second.Data, original) {
		t.Fatal("snapshot data aliases hub storage")
	}
}

func TestFrameHubRejectsInvalidFramesWithoutReplacingSnapshot(t *testing.T) {
	hub := NewFrameHub()
	want := testHubFrame(t, color.Black)
	if err := hub.Publish(want); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		frame monitorJPEGFrame
	}{
		{name: "empty", frame: monitorJPEGFrame{}},
		{name: "not JPEG", frame: monitorJPEGFrame{Data: []byte("not JPEG"), Width: 8, Height: 6}},
		{name: "wrong dimensions", frame: func() monitorJPEGFrame {
			frame := testHubFrame(t, color.White)
			frame.Width++
			return frame
		}()},
		{name: "wrong digest", frame: func() monitorJPEGFrame {
			frame := testHubFrame(t, color.White)
			frame.SHA256[0] ^= 0xff
			return frame
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := hub.Publish(test.frame); err == nil {
				t.Fatal("Publish unexpectedly accepted invalid frame")
			}
		})
	}

	got, ok := hub.Snapshot()
	if !ok || !bytes.Equal(got.Data, want.Data) {
		t.Fatal("invalid publish replaced the last valid snapshot")
	}
}

func TestFrameHubSubscriptionKeepsOnlyLatestFrame(t *testing.T) {
	hub := NewFrameHub()
	first := testHubFrame(t, color.RGBA{R: 255, A: 255})
	second := testHubFrame(t, color.RGBA{G: 255, A: 255})
	third := testHubFrame(t, color.RGBA{B: 255, A: 255})
	if err := hub.Publish(first); err != nil {
		t.Fatal(err)
	}

	subscription := hub.Subscribe(context.Background())
	defer subscription.Close()
	initial := receiveHubFrame(t, subscription.Frames())
	if !bytes.Equal(initial.Data, first.Data) {
		t.Fatal("subscription did not start with the current snapshot")
	}
	if err := hub.Publish(second); err != nil {
		t.Fatal(err)
	}
	if err := hub.Publish(third); err != nil {
		t.Fatal(err)
	}

	latest := receiveHubFrame(t, subscription.Frames())
	if !bytes.Equal(latest.Data, third.Data) {
		t.Fatal("slow subscriber received a stale frame")
	}
}

func TestFrameHubSubscriptionsAreIsolated(t *testing.T) {
	hub := NewFrameHub()
	left := hub.Subscribe(context.Background())
	right := hub.Subscribe(context.Background())
	defer left.Close()
	defer right.Close()

	want := testHubFrame(t, color.RGBA{R: 40, G: 80, B: 120, A: 255})
	if err := hub.Publish(want); err != nil {
		t.Fatal(err)
	}
	leftFrame := receiveHubFrame(t, left.Frames())
	leftFrame.Data[0] ^= 0xff
	rightFrame := receiveHubFrame(t, right.Frames())
	if !bytes.Equal(rightFrame.Data, want.Data) {
		t.Fatal("subscriber frame data is shared with another subscriber")
	}
	snapshot, _ := hub.Snapshot()
	if !bytes.Equal(snapshot.Data, want.Data) {
		t.Fatal("subscriber frame data is shared with hub storage")
	}
}

func TestFrameHubSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	hub := NewFrameHub()
	subscription := hub.Subscribe(context.Background())
	defer subscription.Close()
	frame := testHubFrame(t, color.Black)

	finished := make(chan error, 1)
	go func() {
		for range 100 {
			if err := hub.Publish(frame); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

func TestFrameHubSubscriptionCloseAndContext(t *testing.T) {
	hub := NewFrameHub()
	explicit := hub.Subscribe(context.Background())
	explicit.Close()
	explicit.Close()
	assertHubFramesClosed(t, explicit.Frames())

	ctx, cancel := context.WithCancel(context.Background())
	byContext := hub.Subscribe(ctx)
	cancel()
	assertHubFramesClosed(t, byContext.Frames())

	alreadyCanceled, cancelAlready := context.WithCancel(context.Background())
	cancelAlready()
	immediate := hub.Subscribe(alreadyCanceled)
	assertHubFramesClosed(t, immediate.Frames())
}

func TestFrameHubConcurrentUse(t *testing.T) {
	var hub FrameHub // Exercise zero-value initialization as well.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription := hub.Subscribe(ctx)
	defer subscription.Close()
	frames := []monitorJPEGFrame{
		testHubFrame(t, color.Black),
		testHubFrame(t, color.White),
	}

	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := range 50 {
				if err := hub.Publish(frames[(worker+iteration)%len(frames)]); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
				if snapshot, ok := hub.Snapshot(); !ok || len(snapshot.Data) == 0 {
					t.Error("concurrent Snapshot missed a published frame")
					return
				}
			}
		}()
	}
	wait.Wait()
	if _, ok := hub.Snapshot(); !ok {
		t.Fatal("concurrent hub has no final snapshot")
	}
}

func receiveHubFrame(t testing.TB, frames <-chan monitorJPEGFrame) monitorJPEGFrame {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("frame channel closed unexpectedly")
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame")
		return monitorJPEGFrame{}
	}
}

func assertHubFramesClosed(t testing.TB, frames <-chan monitorJPEGFrame) {
	t.Helper()
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("received a frame after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription to close")
	}
}
