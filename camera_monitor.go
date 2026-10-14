// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

const (
	defaultCameraMonitorIdleTimeout = 15 * time.Second
	cameraMonitorSetupTimeout       = 10 * time.Second
	cameraMonitorStopTimeout        = 5 * time.Second
	cameraMonitorRetryInterval      = time.Second
)

var errCameraMonitorEndedByRemote = errors.New("monitor dialog ended by remote")

type cameraMonitorAgent interface {
	StartMonitor(context.Context) error
	StopMonitor(context.Context) error
	monitorDone() <-chan struct{}
	monitorJPEGEndpoint() (*url.URL, error)
	publishMonitorJPEGFrame(monitorJPEGFrame) error
}

// cameraMonitorController turns HTTP camera demand into one shared outgoing
// monitor dialog. Snapshot requests extend an idle lease; MJPEG requests hold
// a lease until their HTTP connection closes.
type cameraMonitorController struct {
	ctx      context.Context
	agent    cameraMonitorAgent
	idle     time.Duration
	interval time.Duration

	mu         sync.Mutex
	lastDemand time.Time
	streams    int
	wake       chan struct{}
	done       chan struct{}
}

func newCameraMonitorController(ctx context.Context, agent cameraMonitorAgent, idle, interval time.Duration) *cameraMonitorController {
	if idle <= 0 {
		idle = defaultCameraMonitorIdleTimeout
	}
	if interval <= 0 {
		interval = time.Second
	}
	m := &cameraMonitorController{
		ctx:      ctx,
		agent:    agent,
		idle:     idle,
		interval: interval,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	if err := m.publishPlaceholder(); err != nil {
		slog.Warn("Failed to publish camera placeholder", "error", err)
	}
	go m.run()
	return m
}

func (m *cameraMonitorController) TouchSnapshot() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.lastDemand = time.Now()
	m.mu.Unlock()
	m.signal()
}

func (m *cameraMonitorController) AcquireStream() func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	m.streams++
	m.lastDemand = time.Now()
	m.mu.Unlock()
	m.signal()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.streams > 0 {
				m.streams--
			}
			m.lastDemand = time.Now()
			m.mu.Unlock()
			m.signal()
		})
	}
}

func (m *cameraMonitorController) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *cameraMonitorController) demand(now time.Time) (active bool, idleRemaining time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams > 0 {
		return true, m.idle
	}
	if m.lastDemand.IsZero() {
		return false, 0
	}
	remaining := m.idle - now.Sub(m.lastDemand)
	return remaining > 0, remaining
}

func (m *cameraMonitorController) run() {
	defer close(m.done)
	for m.waitForDemand() {
		err := m.runSession()
		if m.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errCameraMonitorEndedByRemote) {
			slog.Info("On-demand camera monitor ended by remote")
			if placeholderErr := m.publishPlaceholder(); placeholderErr != nil {
				slog.Warn("Failed to publish camera placeholder", "error", placeholderErr)
			}
			if !m.suspendUntilDemandReleased() {
				return
			}
			continue
		}
		if err == nil {
			continue
		}
		slog.Warn("On-demand camera monitor stopped", "error", err)
		if active, _ := m.demand(time.Now()); !active {
			continue
		}
		timer := time.NewTimer(cameraMonitorRetryInterval)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
			return
		}
	}
}

// suspendUntilDemandReleased prevents an IFBOX-terminated monitor from being
// redialed while the same snapshot lease or MJPEG connection remains active.
// A later HTTP request starts a fresh monitor session.
func (m *cameraMonitorController) suspendUntilDemandReleased() bool {
	for {
		m.mu.Lock()
		if m.streams == 0 {
			m.lastDemand = time.Time{}
			m.mu.Unlock()
			return true
		}
		m.mu.Unlock()

		select {
		case <-m.wake:
		case <-m.ctx.Done():
			return false
		}
	}
}

func (m *cameraMonitorController) waitForDemand() bool {
	for {
		if active, _ := m.demand(time.Now()); active {
			return true
		}
		select {
		case <-m.wake:
		case <-m.ctx.Done():
			return false
		}
	}
}

func (m *cameraMonitorController) runSession() (retErr error) {
	setupCtx, cancelSetup := context.WithTimeout(m.ctx, cameraMonitorSetupTimeout)
	err := m.agent.StartMonitor(setupCtx)
	cancelSetup()
	if err != nil {
		return fmt.Errorf("start SIP monitor: %w", err)
	}
	slog.Info("On-demand camera monitor started")
	defer func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), cameraMonitorStopTimeout)
		stopErr := m.agent.StopMonitor(stopCtx)
		cancelStop()
		if stopErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("stop SIP monitor: %w", stopErr))
			return
		}
		slog.Info("On-demand camera monitor stopped")
	}()
	monitorDone := m.agent.monitorDone()
	if monitorDone == nil {
		return errors.New("monitor dialog has no completion signal")
	}
	sessionCtx, cancelSession := context.WithCancel(m.ctx)
	defer cancelSession()
	go func() {
		select {
		case <-monitorDone:
			cancelSession()
		case <-sessionCtx.Done():
		}
	}()

	endpoint, err := m.agent.monitorJPEGEndpoint()
	if err != nil {
		return err
	}
	client, transport := newMonitorHTTPClient()
	defer transport.CloseIdleConnections()

	frames := 0
	var baseline [sha256.Size]byte
	for {
		if active, _ := m.demand(time.Now()); !active {
			return nil
		}
		requestStarted := time.Now()
		requestCtx, cancelRequest := context.WithTimeout(sessionCtx, monitorJPEGReqTimeout)
		frame, err := fetchMonitorJPEG(requestCtx, client, endpoint)
		cancelRequest()
		if err != nil {
			if m.ctx.Err() != nil {
				return nil
			}
			if sessionCtx.Err() != nil {
				return errCameraMonitorEndedByRemote
			}
			return err
		}
		if err := m.agent.publishMonitorJPEGFrame(frame); err != nil {
			return err
		}
		frames++
		if frames == 1 {
			baseline = frame.SHA256
		}
		slog.Info("On-demand camera JPEG received",
			"frame", frames,
			"bytes", len(frame.Data),
			"width", frame.Width,
			"height", frame.Height,
			"sha256", fmt.Sprintf("%x", frame.SHA256[:8]),
			"changed", frames > 1 && frame.SHA256 != baseline,
			"duration", frame.Duration,
		)
		if !m.waitForNextPoll(sessionCtx, requestStarted) {
			if m.ctx.Err() == nil && sessionCtx.Err() != nil {
				return errCameraMonitorEndedByRemote
			}
			return nil
		}
	}
}

func (m *cameraMonitorController) waitForNextPoll(ctx context.Context, requestStarted time.Time) bool {
	for {
		active, idleRemaining := m.demand(time.Now())
		if !active {
			return false
		}
		wait := m.interval - time.Since(requestStarted)
		if wait <= 0 {
			return true
		}
		if idleRemaining < wait {
			wait = idleRemaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			return true
		case <-m.wake:
			timer.Stop()
			continue
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

func (m *cameraMonitorController) publishPlaceholder() error {
	frame, err := newCameraPlaceholderFrame()
	if err != nil {
		return fmt.Errorf("create camera placeholder: %w", err)
	}
	return m.agent.publishMonitorJPEGFrame(frame)
}

func newCameraPlaceholderFrame() (monitorJPEGFrame, error) {
	const width, height = 640, 480
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 75}); err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("encode black JPEG: %w", err)
	}
	bytes := data.Bytes()
	return monitorJPEGFrame{
		Data:   bytes,
		Width:  width,
		Height: height,
		SHA256: sha256.Sum256(bytes),
	}, nil
}
