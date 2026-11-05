// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

func (h *HTTPServer) handleTalkbackState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.agent == nil || h.agent.talkback == nil {
		http.Error(w, "talkback unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.agent.talkback.Snapshot())
}

func (h *HTTPServer) handleTalkbackPCMU(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.agent == nil || h.agent.talkback == nil {
		http.Error(w, "talkback unavailable", http.StatusServiceUnavailable)
		return
	}
	release, err := h.agent.talkback.BeginInput()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer release()
	releaseMonitor := h.camera.AcquireAudioStream()
	defer releaseMonitor()

	// FFmpeg uploads the microphone as one long-lived chunked request. Keep the
	// response pending until its stdin closes; an early final response can make
	// some HTTP output clients stop transmitting.
	frame := make([]byte, audioPCMUFrameBytes)
	frames := 0
	bytesRead := 0
	for {
		n, err := io.ReadFull(r.Body, frame)
		bytesRead += n
		if n == audioPCMUFrameBytes {
			h.agent.talkback.Publish(frame)
			frames++
		}
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF && r.Context().Err() == nil {
				slog.Warn("Talkback HTTP input stopped", "error", err, "frames", frames, "bytes", bytesRead)
			} else {
				slog.Info("Talkback HTTP input stopped", "frames", frames, "bytes", bytesRead)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
}
