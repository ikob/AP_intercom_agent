// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type HTTPServer struct {
	srv    *http.Server
	agent  *Agent
	cfg    *Config
	msg    *MessageBuffer
	camera *cameraMonitorController
}

func StartHTTP(ctx context.Context, listen string, agent *Agent, cfg *Config, msg *MessageBuffer) *HTTPServer {
	if listen == "" {
		return nil
	}

	h := &HTTPServer{
		agent: agent,
		cfg:   cfg,
		msg:   msg,
	}
	if agent != nil && agent.cfg.MonitorURI != "" {
		h.camera = newCameraMonitorController(
			ctx,
			agent,
			defaultCameraMonitorIdleTimeout,
			agent.cfg.MonitorJPEGInterval.Duration,
		)
		agent.cameraMonitor = h.camera
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/v1/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		publicConfig := *cfg
		passwordConfigured := publicConfig.Password != ""
		ringWebhookConfigured := publicConfig.RingWebhookURL != ""
		publicConfig.Password = ""
		publicConfig.RingWebhookURL = ""

		resp := map[string]any{
			"config_path":             cfg.LoadedConfigPath,
			"config":                  &publicConfig,
			"password_configured":     passwordConfigured,
			"ring_webhook_configured": ringWebhookConfigured,
			"runtime": map[string]any{
				"answer_calls":   agent.answerEnabled.Load(),
				"send_messages":  agent.messageEnabled.Load(),
				"capture_images": agent.captureEnabled.Load(),
			},
		}

		b, err := SaveConfigJSON(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})

	mux.HandleFunc("/v1/state", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"answer_calls":   agent.answerEnabled.Load(),
				"send_messages":  agent.messageEnabled.Load(),
				"capture_images": agent.captureEnabled.Load(),
			})
			return

		case http.MethodPost:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()

			// pointer bool
			var req struct {
				AnswerCalls   *bool `json:"answer_calls"`
				SendMessages  *bool `json:"send_messages"`
				CaptureImages *bool `json:"capture_images"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}

			prevAnswer := agent.answerEnabled.Load()
			prevMessages := agent.messageEnabled.Load()
			prevCapture := agent.captureEnabled.Load()
			changed := false

			if req.AnswerCalls != nil {
				agent.SetAnswerEnabled(*req.AnswerCalls)
				changed = true
			}
			if req.SendMessages != nil {
				agent.SetMessageEnabled(*req.SendMessages)
				changed = true
			}
			if req.CaptureImages != nil {
				agent.SetCaptureEnabled(*req.CaptureImages)
				changed = true
			}

			if changed {
				slog.Info("HTTP state updated",
					"prev_answer_calls", prevAnswer,
					"prev_send_messages", prevMessages,
					"prev_capture_images", prevCapture,
					"answer_calls", agent.answerEnabled.Load(),
					"send_messages", agent.messageEnabled.Load(),
					"capture_images", agent.captureEnabled.Load(),
				)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"answer_calls":   agent.answerEnabled.Load(),
				"send_messages":  agent.messageEnabled.Load(),
				"capture_images": agent.captureEnabled.Load(),
			})
			return

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})

	mux.HandleFunc("/v1/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agent.currentCallSnapshot())
	})
	mux.HandleFunc("/v1/call/answer", h.callActionHandler(incomingCallAnswer))
	mux.HandleFunc("/v1/call/hangup", h.callActionHandler(incomingCallHangup))

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg.Snapshot())
	})
	mux.HandleFunc("/v1/camera.jpg", h.handleCameraSnapshot)
	mux.HandleFunc("/v1/camera.mjpeg", h.handleCameraMJPEG)
	mux.HandleFunc("/v1/camera/state", h.handleCameraState)
	mux.HandleFunc("/v1/camera/reconnect", h.handleCameraReconnect)
	mux.HandleFunc("/v1/audio.pcmu", h.handleAudioPCMU)

	h.srv = &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		slog.Info("HTTP server listening", "addr", listen)
		if err := h.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shCtx)
	}()

	return h
}

func (h *HTTPServer) callActionHandler(command incomingCallCommand) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			SessionID string `json:"session_id"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		if err := decoder.Decode(&request); err != nil || request.SessionID == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		if err := h.agent.enqueueIncomingCallCommand(request.SessionID, command); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errNoActiveCall) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true,
			"call":     h.agent.currentCallSnapshot(),
		})
	}
}

func (h *HTTPServer) WaitCameraStopped(ctx context.Context) error {
	if h == nil || h.camera == nil {
		return nil
	}
	select {
	case <-h.camera.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
