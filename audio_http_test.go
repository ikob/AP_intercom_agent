// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestHTTPAudioPCMUStreamsSilenceAndReceivedAudio(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/v1/audio.pcmu", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "audio/basic" {
		t.Fatalf("Content-Type = %q", got)
	}

	first := make([]byte, audioPCMUFrameBytes)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, bytes.Repeat([]byte{audioPCMUSilence}, audioPCMUFrameBytes)) {
		t.Fatal("first frame is not PCMU silence")
	}

	want := bytes.Repeat([]byte{0x55}, audioPCMUFrameBytes)
	agent.audioHub.Publish(want)
	got := make([]byte, audioPCMUFrameBytes)
	if _, err := io.ReadFull(response.Body, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("received frame starts %x, want %x", got[:8], want[:8])
	}
}

func TestHTTPAudioPCMURejectsWrites(t *testing.T) {
	baseURL, cancel, _, _, _ := startHTTPTestServer(t)
	defer cancel()
	response, err := http.Post(baseURL+"/v1/audio.pcmu", "application/octet-stream", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
