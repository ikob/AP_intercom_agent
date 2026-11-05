// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestHTTPTalkbackStreamsPCMUToActiveTarget(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()
	writer := newRecordingTalkbackWriter()
	agent.talkback.SetTarget("monitor:http-test", writer)
	voice := bytes.Repeat([]byte{0x33}, audioPCMUFrameBytes)
	reader, writerPipe := io.Pipe()
	responseDone := make(chan *http.Response, 1)
	errorDone := make(chan error, 1)
	go func() {
		response, err := http.Post(baseURL+"/v1/talk.pcmu", "audio/basic", reader)
		if err != nil {
			errorDone <- err
			return
		}
		responseDone <- response
	}()
	if _, err := writerPipe.Write(voice); err != nil {
		t.Fatal(err)
	}
	writer.waitFor(t, func(frame []byte) bool { return bytes.Equal(frame, voice) })
	if err := writerPipe.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errorDone:
		t.Fatal(err)
	case response := <-responseDone:
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusNoContent)
		}
	}

	stateResponse, err := http.Get(baseURL + "/v1/talk")
	if err != nil {
		t.Fatal(err)
	}
	defer stateResponse.Body.Close()
	var state talkbackSnapshot
	if err := json.NewDecoder(stateResponse.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if !state.Available || state.TargetID != "monitor:http-test" || state.InputActive {
		t.Fatalf("state = %+v", state)
	}
}

func TestHTTPTalkbackAllowsConcurrentInput(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()
	writer := newRecordingTalkbackWriter()
	agent.talkback.SetTarget("monitor:concurrent-test", writer)
	release, err := agent.talkback.BeginInput()
	if err != nil {
		t.Fatal(err)
	}
	voice := bytes.Repeat([]byte{0x44}, audioPCMUFrameBytes)

	response, err := http.Post(baseURL+"/v1/talk.pcmu", "audio/basic", bytes.NewReader(voice))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	writer.waitFor(t, func(frame []byte) bool { return bytes.Equal(frame, voice) })
	if !agent.talkback.Snapshot().InputActive {
		t.Fatal("placeholder input was released by concurrent input")
	}
	release()
	if agent.talkback.Snapshot().InputActive {
		t.Fatal("input is still active after final release")
	}
}
