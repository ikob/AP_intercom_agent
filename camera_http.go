// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const cameraMJPEGBoundary = "ap_intercom_agent_frame"

func (h *HTTPServer) handleCameraSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.agent == nil || h.agent.frameHub == nil {
		cameraUnavailable(w)
		return
	}
	if r.Method == http.MethodGet && h.camera != nil {
		h.camera.TouchSnapshot()
	}
	frame, ok := h.agent.frameHub.Snapshot()
	if !ok {
		cameraUnavailable(w)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(frame.Data)))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("ETag", fmt.Sprintf("\"%x\"", frame.SHA256))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(frame.Data)
}

func (h *HTTPServer) handleCameraMJPEG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.agent == nil || h.agent.frameHub == nil {
		cameraUnavailable(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	subscription := h.agent.frameHub.Subscribe(r.Context())
	defer subscription.Close()
	releaseMonitor := h.camera.AcquireStream()
	defer releaseMonitor()

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+cameraMJPEGBoundary)
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case frame, open := <-subscription.Frames():
			if !open {
				return
			}
			if _, err := fmt.Fprintf(
				w,
				"--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\nX-Frame-SHA256: %x\r\n\r\n",
				cameraMJPEGBoundary,
				len(frame.Data),
				frame.SHA256,
			); err != nil {
				return
			}
			if _, err := w.Write(frame.Data); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func cameraUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "camera frame unavailable", http.StatusServiceUnavailable)
}
