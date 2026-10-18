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

	"github.com/emiago/sipgo"
)

const (
	defaultCameraMonitorIdleTimeout = 15 * time.Second
	cameraMonitorSetupTimeout       = 10 * time.Second
	cameraMonitorStopTimeout        = 5 * time.Second
	cameraMonitorRetryInterval      = time.Second
	cameraMonitorRetryBase          = 3 * time.Second
	cameraMonitorRetryMax           = 10 * time.Second
	cameraMonitorStableSession      = 10 * time.Second
)

var errCameraMonitorEndedByRemote = errors.New("monitor dialog ended by remote")

const (
	cameraStateIdle          = "idle"
	cameraStateConnecting    = "connecting"
	cameraStateActive        = "active"
	cameraStateEndedByRemote = "ended_by_remote"
	cameraStateSuspended     = "suspended"
)

type cameraMonitorSnapshot struct {
	State              string `json:"state"`
	MonitorActive      bool   `json:"monitor_active"`
	DemandActive       bool   `json:"demand_active"`
	ConsumerCount      int    `json:"consumer_count"`
	AudioConsumerCount int    `json:"audio_consumer_count"`
	ReconnectAvailable bool   `json:"reconnect_available"`
	Suspended          bool   `json:"suspended"`
}

type cameraMonitorAgent interface {
	StartMonitor(context.Context) error
	StartMonitorAudio(context.Context) error
	StopMonitor(context.Context) error
	monitorDone() <-chan struct{}
	monitorJPEGEndpoint() (*url.URL, error)
	publishMonitorJPEGFrame(monitorJPEGFrame) error
}

// cameraMonitorController turns HTTP camera demand into one shared outgoing
// monitor dialog. Snapshot requests extend an idle lease; MJPEG requests hold
// a lease until their HTTP connection closes.
type cameraMonitorController struct {
	ctx       context.Context
	agent     cameraMonitorAgent
	idle      time.Duration
	interval  time.Duration
	retryBase time.Duration
	retryMax  time.Duration

	mu                sync.Mutex
	lastDemand        time.Time
	streams           int
	audioStreams      int
	demandGeneration  uint64
	remoteEnded       bool
	sessionActive     bool
	suspended         bool
	suspendGeneration uint64
	wake              chan struct{}
	done              chan struct{}
}

func newCameraMonitorController(ctx context.Context, agent cameraMonitorAgent, idle, interval time.Duration) *cameraMonitorController {
	if idle <= 0 {
		idle = defaultCameraMonitorIdleTimeout
	}
	if interval <= 0 {
		interval = time.Second
	}
	m := &cameraMonitorController{
		ctx:       ctx,
		agent:     agent,
		idle:      idle,
		interval:  interval,
		retryBase: cameraMonitorRetryBase,
		retryMax:  cameraMonitorRetryMax,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
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
	now := time.Now()
	if active, _ := m.rawDemandLocked(now); !active {
		m.demandGeneration++
		m.remoteEnded = false
	}
	m.lastDemand = now
	m.mu.Unlock()
	m.signal()
}

func (m *cameraMonitorController) AcquireStream() func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	now := time.Now()
	if active, _ := m.rawDemandLocked(now); m.remoteEnded || (m.streams == 0 && !active) {
		m.demandGeneration++
		m.remoteEnded = false
	}
	m.streams++
	m.lastDemand = now
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

// AcquireAudioStream holds the shared SIP monitor exactly like a video stream,
// while also requesting the IFBOX talking mode that enables RTP audio.
func (m *cameraMonitorController) AcquireAudioStream() func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	now := time.Now()
	if active, _ := m.rawDemandLocked(now); m.remoteEnded || (m.streams == 0 && !active) {
		m.demandGeneration++
		m.remoteEnded = false
	}
	m.streams++
	m.audioStreams++
	m.lastDemand = now
	m.mu.Unlock()
	m.signal()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.streams > 0 {
				m.streams--
			}
			if m.audioStreams > 0 {
				m.audioStreams--
			}
			m.lastDemand = time.Now()
			m.mu.Unlock()
			m.signal()
		})
	}
}

func (m *cameraMonitorController) audioDemand() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.suspended && !m.remoteEnded && m.audioStreams > 0
}

func (m *cameraMonitorController) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Suspend preserves HTTP camera demand while preventing an outgoing SIP
// monitor dialog. Incoming calls use it before preempting the current monitor.
func (m *cameraMonitorController) Suspend() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.suspended {
		m.suspended = true
		m.suspendGeneration++
	}
	m.mu.Unlock()
	m.signal()
}

// Resume lets the controller recreate the monitor dialog when the existing
// snapshot lease or MJPEG viewer still requires it.
func (m *cameraMonitorController) Resume() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.suspended = false
	m.mu.Unlock()
	m.signal()
}

func (m *cameraMonitorController) Suspended() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspended
}

func (m *cameraMonitorController) preemptionGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspendGeneration
}

func (m *cameraMonitorController) preemptedSince(generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.suspendGeneration != generation
}

func (m *cameraMonitorController) demand(now time.Time) (active bool, idleRemaining time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.suspended || m.remoteEnded {
		return false, 0
	}
	return m.rawDemandLocked(now)
}

func (m *cameraMonitorController) rawDemandLocked(now time.Time) (active bool, idleRemaining time.Duration) {
	if m.streams > 0 {
		return true, m.idle
	}
	if m.lastDemand.IsZero() {
		return false, 0
	}
	remaining := m.idle - now.Sub(m.lastDemand)
	return remaining > 0, remaining
}

func (m *cameraMonitorController) currentDemandGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.demandGeneration
}

func (m *cameraMonitorController) setSessionActive(active bool) {
	m.mu.Lock()
	m.sessionActive = active
	m.mu.Unlock()
}

func (m *cameraMonitorController) Snapshot() cameraMonitorSnapshot {
	if m == nil {
		return cameraMonitorSnapshot{State: "unavailable"}
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	demandActive, _ := m.rawDemandLocked(now)
	state := cameraStateIdle
	switch {
	case m.suspended:
		state = cameraStateSuspended
	case m.remoteEnded && demandActive:
		state = cameraStateEndedByRemote
	case m.sessionActive:
		state = cameraStateActive
	case demandActive:
		state = cameraStateConnecting
	}
	return cameraMonitorSnapshot{
		State:              state,
		MonitorActive:      m.sessionActive,
		DemandActive:       demandActive,
		ConsumerCount:      m.streams,
		AudioConsumerCount: m.audioStreams,
		ReconnectAvailable: m.remoteEnded,
		Suspended:          m.suspended,
	}
}

// RequestReconnect starts a new demand generation after the IFBOX ended the
// preceding confirmed dialog. If the old consumers have already gone away, a
// short snapshot lease is created so the explicit request still starts a new
// monitor dialog.
func (m *cameraMonitorController) RequestReconnect() bool {
	if m == nil {
		return false
	}
	now := time.Now()
	m.mu.Lock()
	if !m.remoteEnded {
		m.mu.Unlock()
		return false
	}
	active, _ := m.rawDemandLocked(now)
	m.demandGeneration++
	m.remoteEnded = false
	if !active {
		m.lastDemand = now
	}
	m.mu.Unlock()
	m.signal()
	return true
}

// blockAfterRemoteEnd prevents a confirmed monitor dialog that the IFBOX ended
// from being recreated by the same outstanding HTTP/RTSP demand. A later demand
// generation clears the block in TouchSnapshot, AcquireStream, or
// AcquireAudioStream.
func (m *cameraMonitorController) blockAfterRemoteEnd(generation uint64) {
	m.mu.Lock()
	m.sessionActive = false
	if m.demandGeneration == generation {
		m.remoteEnded = true
	}
	m.mu.Unlock()
	m.signal()
}

func (m *cameraMonitorController) run() {
	defer close(m.done)
	backoffFailures := 0
	for m.waitForDemand() {
		sessionStarted := time.Now()
		err := m.runSession()
		if m.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errCameraMonitorEndedByRemote) {
			if m.Suspended() {
				backoffFailures = 0
				continue
			}
			slog.Info("On-demand camera monitor ended by remote")
			backoffFailures = 0
			continue
		}
		if err == nil {
			backoffFailures = 0
			continue
		}
		if active, _ := m.demand(time.Now()); !active {
			backoffFailures = 0
			continue
		}

		delay := cameraMonitorRetryInterval
		if cameraMonitorNeedsBackoff(err) {
			if time.Since(sessionStarted) >= cameraMonitorStableSession {
				backoffFailures = 0
			}
			backoffFailures++
			delay = cameraMonitorRetryDelay(backoffFailures, m.retryBase, m.retryMax)
		} else {
			backoffFailures = 0
		}
		slog.Warn("On-demand camera monitor stopped", "error", err, "retry_in", delay, "backoff_failures", backoffFailures)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func cameraMonitorNeedsBackoff(err error) bool {
	var responseErr *sipgo.ErrDialogResponse
	return errors.As(err, &responseErr) && responseErr.Res != nil && responseErr.Res.StatusCode == 603
}

func cameraMonitorRetryDelay(failures int, base, maximum time.Duration) time.Duration {
	if base <= 0 {
		base = cameraMonitorRetryBase
	}
	if maximum < base {
		maximum = base
	}
	if failures < 1 {
		failures = 1
	}
	delay := base
	for attempt := 1; attempt < failures; attempt++ {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
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
	preemptionGeneration := m.preemptionGeneration()
	demandGeneration := m.currentDemandGeneration()
	setupCtx, cancelSetup := context.WithTimeout(m.ctx, cameraMonitorSetupTimeout)
	err := m.agent.StartMonitor(setupCtx)
	cancelSetup()
	if err != nil {
		return fmt.Errorf("start SIP monitor: %w", err)
	}
	m.setSessionActive(true)
	defer m.setSessionActive(false)
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
	audioStarted := false
	var baseline [sha256.Size]byte
	for {
		if active, _ := m.demand(time.Now()); !active {
			return nil
		}
		if !audioStarted && m.audioDemand() {
			audioCtx, cancelAudio := context.WithTimeout(sessionCtx, cameraMonitorSetupTimeout)
			err := m.agent.StartMonitorAudio(audioCtx)
			cancelAudio()
			if err != nil {
				return fmt.Errorf("start SIP monitor audio: %w", err)
			}
			audioStarted = true
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
				if m.preemptedSince(preemptionGeneration) {
					return nil
				}
				m.blockAfterRemoteEnd(demandGeneration)
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
				if m.preemptedSince(preemptionGeneration) {
					return nil
				}
				m.blockAfterRemoteEnd(demandGeneration)
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
