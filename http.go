// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type HTTPServer struct {
	srv   *http.Server
	agent *Agent
	cfg   *Config
	msg   *MessageBuffer
}

// StartHTTP launches the optional HTTP control plane and shuts it down on context cancel.
func StartHTTP(ctx context.Context, listen string, agent *Agent, cfg *Config, msg *MessageBuffer) *HTTPServer {
	if listen == "" {
		return nil
	}

	h := &HTTPServer{
		agent: agent,
		cfg:   cfg,
		msg:   msg,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/v1/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		resp := map[string]any{
			"config_path": cfg.LoadedConfigPath,
			"config":      cfg,
			"runtime": map[string]any{
				"answer_calls":   agent.answerEnabled.Load(),
				"send_massage":   agent.messageEnabled.Load(),
				"answer_message": agent.answerMessageEnabled.Load(),
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
				"send_massage":   agent.messageEnabled.Load(),
				"answer_message": agent.answerMessageEnabled.Load(),
			})
			return

		case http.MethodPost:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()

			// pointer bool
			var req struct {
				AnswerCalls   *bool `json:"answer_calls"`
				SendMassage   *bool `json:"send_massage"`
				AnswerMessage *bool `json:"answer_message"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}

			prevAnswer := agent.answerEnabled.Load()
			prevMessages := agent.messageEnabled.Load()
			prevAnswerMessage := agent.answerMessageEnabled.Load()
			changed := false

			if req.AnswerCalls != nil {
				agent.SetAnswerEnabled(*req.AnswerCalls)
				changed = true
			}
			if req.SendMassage != nil {
				agent.SetMessageEnabled(*req.SendMassage)
				changed = true
			}
			if req.AnswerMessage != nil {
				agent.SetAnswerMessageEnabled(*req.AnswerMessage)
				changed = true
			}

			if changed {
				slog.Info("HTTP state updated",
					"prev_answer_calls", prevAnswer,
					"prev_send_massage", prevMessages,
					"prev_answer_message", prevAnswerMessage,
					"answer_calls", agent.answerEnabled.Load(),
					"send_massage", agent.messageEnabled.Load(),
					"answer_message", agent.answerMessageEnabled.Load(),
				)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"answer_calls":   agent.answerEnabled.Load(),
				"send_massage":   agent.messageEnabled.Load(),
				"answer_message": agent.answerMessageEnabled.Load(),
			})
			return

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	})

	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg.Snapshot())
	})

	h.srv = &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
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
