// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"net/http"
	"time"
)

const (
	audioPCMUFrameInterval = 20 * time.Millisecond
	audioPCMUFrameBytes    = 160 // G.711 PCMU, 8 kHz, mono, 20 ms
	audioPCMUSilence       = 0xff
)

func (h *HTTPServer) handleAudioPCMU(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.agent == nil || h.agent.audioHub == nil {
		http.Error(w, "audio unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	subscription := h.agent.audioHub.Subscribe(r.Context())
	defer subscription.Close()
	releaseMonitor := h.camera.AcquireAudioStream()
	defer releaseMonitor()

	w.Header().Set("Content-Type", "audio/basic")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	silence := bytes.Repeat([]byte{audioPCMUSilence}, audioPCMUFrameBytes)
	ticker := time.NewTicker(audioPCMUFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			packet := silence
			select {
			case received, open := <-subscription.Packets():
				if !open {
					return
				}
				packet = normalizePCMUFrame(received, silence)
			default:
			}
			if _, err := w.Write(packet); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func normalizePCMUFrame(packet, silence []byte) []byte {
	if len(packet) == audioPCMUFrameBytes {
		return packet
	}
	frame := append([]byte(nil), silence...)
	copy(frame, packet)
	return frame
}
