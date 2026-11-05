// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Opt-in integration test: TALKBACK_FFMPEG_TEST=1 go test -run
// TestTalkbackFFmpegPCMAToPCMU -v. It never connects to IFBOX.
func TestTalkbackFFmpegPCMAToPCMU(t *testing.T) {
	if os.Getenv("TALKBACK_FFMPEG_TEST") == "" {
		t.Skip("set TALKBACK_FFMPEG_TEST=1 to test the FFmpeg talkback converter")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal(err)
	}
	baseURL, cancelServer, agent, _, _ := startHTTPTestServer(t)
	defer cancelServer()
	target := newRecordingTalkbackWriter()
	agent.talkback.SetTarget("monitor:ffmpeg-test", target)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "alaw", "-ar", "8000", "-ac", "1", "-i", "pipe:0",
		"-c:a", "pcm_mulaw", "-ar", "8000", "-ac", "1",
		"-flush_packets", "1", "-f", "mulaw", "-method", "POST",
		baseURL+"/v1/talk.pcmu",
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte{0x2a}, audioPCMUFrameBytes*50)
	if _, err := stdin.Write(input); err != nil {
		t.Fatal(err)
	}
	target.waitFor(t, func(frame []byte) bool {
		return len(frame) == audioPCMUFrameBytes && !bytes.Equal(frame, talkbackSilenceFrame)
	})
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}
