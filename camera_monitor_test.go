// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"crypto/sha256"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCameraMonitorAgent struct {
	endpoint *url.URL
	hub      *FrameHub
	starts   atomic.Int32
	stops    atomic.Int32
	mu       sync.Mutex
	done     chan struct{}
}

func (a *fakeCameraMonitorAgent) StartMonitor(context.Context) error {
	a.starts.Add(1)
	a.mu.Lock()
	a.done = make(chan struct{})
	a.mu.Unlock()
	return nil
}

func (a *fakeCameraMonitorAgent) StopMonitor(context.Context) error {
	a.stops.Add(1)
	return nil
}

func (a *fakeCameraMonitorAgent) monitorDone() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done
}

func (a *fakeCameraMonitorAgent) endMonitorFromRemote() {
	a.mu.Lock()
	defer a.mu.Unlock()
	close(a.done)
}

func (a *fakeCameraMonitorAgent) monitorJPEGEndpoint() (*url.URL, error) {
	return a.endpoint, nil
}

func (a *fakeCameraMonitorAgent) publishMonitorJPEGFrame(frame monitorJPEGFrame) error {
	return a.hub.Publish(frame)
}

func TestCameraMonitorControllerOnDemandLifecycle(t *testing.T) {
	first := testJPEG(t, color.RGBA{R: 0x20, G: 0x40, B: 0x60, A: 0xff})
	second := testJPEG(t, color.RGBA{R: 0x60, G: 0x40, B: 0x20, A: 0xff})
	var requests atomic.Int32
	jpegServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "image/jpeg")
		if requests.Add(1)%2 == 1 {
			_, _ = response.Write(first)
			return
		}
		_, _ = response.Write(second)
	}))
	defer jpegServer.Close()
	endpoint, err := url.Parse(jpegServer.URL + monitorJPEGPath + "?s=test")
	if err != nil {
		t.Fatal(err)
	}

	hub := NewFrameHub()
	fake := &fakeCameraMonitorAgent{endpoint: endpoint, hub: hub}
	ctx, cancel := context.WithCancel(context.Background())
	controller := newCameraMonitorController(ctx, fake, 80*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-controller.done
	})

	placeholder, ok := hub.Snapshot()
	if !ok || placeholder.Width != 640 || placeholder.Height != 480 {
		t.Fatalf("camera placeholder = %dx%d present=%v", placeholder.Width, placeholder.Height, ok)
	}
	controller.TouchSnapshot()
	waitForAtomicAtLeast(t, &fake.starts, 1)
	waitForAtomicAtLeast(t, &requests, 1)

	controller.TouchSnapshot()
	controller.TouchSnapshot()
	release := controller.AcquireStream()
	time.Sleep(2 * 80 * time.Millisecond)
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("shared demand started %d monitor dialogs, want 1", got)
	}
	if got := fake.stops.Load(); got != 0 {
		t.Fatalf("active stream stopped monitor %d times", got)
	}

	release()
	waitForAtomicAtLeast(t, &fake.stops, 1)
	controller.TouchSnapshot()
	waitForAtomicAtLeast(t, &fake.starts, 2)
}

func TestCameraMonitorControllerDoesNotRedialAfterRemoteEndUntilNewDemand(t *testing.T) {
	jpegData := testJPEG(t, color.RGBA{R: 0x20, G: 0x40, B: 0x60, A: 0xff})
	var requests atomic.Int32
	jpegServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "image/jpeg")
		_, _ = response.Write(jpegData)
	}))
	defer jpegServer.Close()
	endpoint, err := url.Parse(jpegServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	hub := NewFrameHub()
	fake := &fakeCameraMonitorAgent{endpoint: endpoint, hub: hub}
	ctx, cancel := context.WithCancel(context.Background())
	controller := newCameraMonitorController(ctx, fake, 50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-controller.done
	})

	release := controller.AcquireStream()
	waitForAtomicAtLeast(t, &fake.starts, 1)
	waitForAtomicAtLeast(t, &requests, 1)
	waitForFrameSHA256(t, hub, sha256.Sum256(jpegData))
	fake.endMonitorFromRemote()
	waitForAtomicAtLeast(t, &fake.stops, 1)

	time.Sleep(cameraMonitorRetryInterval + 100*time.Millisecond)
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("remote end restarted monitor %d times, want 1", got)
	}
	placeholder, ok := hub.Snapshot()
	wantPlaceholder, placeholderErr := newCameraPlaceholderFrame()
	if placeholderErr != nil {
		t.Fatal(placeholderErr)
	}
	if !ok || placeholder.SHA256 != wantPlaceholder.SHA256 {
		t.Fatal("remote end did not replace the live frame with a placeholder")
	}

	release()
	time.Sleep(2 * 50 * time.Millisecond)
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("released demand restarted monitor %d times, want 1", got)
	}

	releaseAgain := controller.AcquireStream()
	defer releaseAgain()
	waitForAtomicAtLeast(t, &fake.starts, 2)
}

func TestCameraHTTPGetTriggersMonitorButHeadDoesNot(t *testing.T) {
	jpegData := testJPEG(t, color.Black)
	jpegServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "image/jpeg")
		_, _ = response.Write(jpegData)
	}))
	defer jpegServer.Close()
	endpoint, err := url.Parse(jpegServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	hub := NewFrameHub()
	fake := &fakeCameraMonitorAgent{endpoint: endpoint, hub: hub}
	ctx, cancel := context.WithCancel(context.Background())
	controller := newCameraMonitorController(ctx, fake, 50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-controller.done
	})
	handler := &HTTPServer{
		agent:  &Agent{frameHub: hub},
		camera: controller,
	}
	server := httptest.NewServer(http.HandlerFunc(handler.handleCameraSnapshot))
	defer server.Close()

	request, err := http.NewRequest(http.MethodHead, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	time.Sleep(20 * time.Millisecond)
	if got := fake.starts.Load(); got != 0 {
		t.Fatalf("HEAD started %d monitor dialogs", got)
	}

	response, err = http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	waitForAtomicAtLeast(t, &fake.starts, 1)
}

func waitForAtomicAtLeast(t *testing.T, value *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("value = %d, want at least %d", value.Load(), want)
}

func waitForFrameSHA256(t *testing.T, hub *FrameHub, want [sha256.Size]byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frame, ok := hub.Snapshot()
		if ok && frame.SHA256 == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("camera frame SHA-256 did not become %x", want[:8])
}
