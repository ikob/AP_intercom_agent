// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const cameraRTSPFrameInterval = time.Second / 2

// Opt-in integration test: GO2RTC_BIN=/path/to/go2rtc go test -run TestCameraRTSP -v
// Uses the real MJPEG handler and synthetic JPEGs; never registers with IFBOX.
func TestCameraRTSP(t *testing.T) {
	binary := os.Getenv("GO2RTC_BIN")
	if binary == "" {
		t.Skip("set GO2RTC_BIN to test the FFmpeg/go2rtc pipeline")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatal(err)
	}
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()
	frame, err := newCameraPlaceholderFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.frameHub.Publish(frame); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		ticker := time.NewTicker(cameraRTSPFrameInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = agent.frameHub.Publish(frame)
			case <-ctx.Done():
				return
			}
		}
	}()
	// Reserve independent ephemeral ports before starting the subprocess.
	ports := make([]string, 3)
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = l.Addr().String()
		_ = l.Close()
	}
	config, err := os.ReadFile("go2rtc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.NewReplacer(
		"127.0.0.1:1984", ports[0],
		"127.0.0.1:8554", ports[1],
		":8555", ports[2],
		"http://127.0.0.1:18080", baseURL,
	).Replace(string(config))
	path := filepath.Join(t.TempDir(), "go2rtc.yaml")
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.CreateTemp(t.TempDir(), "go2rtc-log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, binary, "-config", path)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop()
		_ = cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(log.Name())
			t.Log(string(b))
		}
	}()
	wait := func(label string, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", label)
	}
	wait("RTSP listener", func() bool {
		c, err := net.DialTimeout("tcp", ports[1], 100*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	wait("WebRTC listener", func() bool {
		c, err := net.DialTimeout("tcp", ports[2], 100*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	subscribers := func() int {
		agent.frameHub.mu.RLock()
		defer agent.frameHub.mu.RUnlock()
		return len(agent.frameHub.subscribers)
	}
	if n := subscribers(); n != 0 {
		t.Fatalf("idle subscribers = %d", n)
	}
	// Two long-running clients must share a single MJPEG upstream. Closing one
	// must not release the other viewer's lease.
	clients := make([]context.CancelFunc, 2)
	done := make([]chan error, 2)
	for i := range clients {
		clientCtx, cancelClient := context.WithCancel(ctx)
		clients[i] = cancelClient
		defer cancelClient()
		done[i] = make(chan error, 1)
		client := exec.CommandContext(clientCtx, "ffmpeg", "-v", "error", "-rtsp_transport", "tcp", "-probesize", "32", "-analyzeduration", "0", "-threads", "1", "-flags", "low_delay", "-i", fmt.Sprintf("rtsp://%s/intercom", ports[1]), "-f", "null", "-")
		if err := client.Start(); err != nil {
			t.Fatal(err)
		}
		go func(ch chan error) { ch <- client.Wait() }(done[i])
	}
	wait("shared MJPEG subscription", func() bool { return subscribers() == 1 })
	// Allow both clients to finish RTSP setup before exercising partial teardown.
	time.Sleep(3 * time.Second)
	if n := subscribers(); n != 1 {
		t.Fatalf("concurrent upstreams = %d, want 1", n)
	}
	for i := range clients {
		select {
		case err := <-done[i]:
			t.Fatalf("viewer %d exited early: %v", i, err)
		default:
		}
	}
	clients[0]()
	<-done[0]
	time.Sleep(time.Second)
	if n := subscribers(); n != 1 {
		t.Fatalf("upstreams with one remaining viewer = %d", n)
	}
	clients[1]()
	<-done[1]
	wait("last viewer lease release", func() bool { return subscribers() == 0 })

	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	probe := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp", "-probesize", "1000000", "-analyzeduration", "3000000", "-i", fmt.Sprintf("rtsp://%s/intercom", ports[1]), "-select_streams", "v:0", "-read_intervals", "%+3", "-show_entries", "packet=pts_time,duration_time,flags", "-of", "json")
	rate, err := probe.Output()
	cancelProbe()
	if err != nil {
		t.Fatalf("probe frame rate: %v", err)
	}
	var packets struct {
		Packets []struct {
			PTS      string `json:"pts_time"`
			Duration string `json:"duration_time"`
			Flags    string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(rate, &packets); err != nil {
		t.Fatal(err)
	}
	if len(packets.Packets) < 2 {
		t.Fatalf("too few packets: %s", rate)
	}
	checkedIntervals := 0
	for _, packet := range packets.Packets {
		duration, err := strconv.ParseFloat(packet.Duration, 64)
		if err != nil {
			t.Fatal(err)
		}
		if duration < 0.49 || duration > 0.51 {
			t.Fatalf("packet duration = %q, want about half a second", packet.Duration)
		}
		if !strings.Contains(packet.Flags, "K") {
			t.Fatalf("packet flags = %q, want every frame to be a keyframe", packet.Flags)
		}
	}
	for i := 1; i < len(packets.Packets); i++ {
		if packets.Packets[i-1].PTS == "" || packets.Packets[i].PTS == "" {
			continue
		}
		checkedIntervals++
		a, err := strconv.ParseFloat(packets.Packets[i-1].PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		b, err := strconv.ParseFloat(packets.Packets[i].PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		if delta := b - a; delta < 0.49 || delta > 0.51 {
			t.Fatalf("packet interval = %f, want about half a second", delta)
		}
	}

	if checkedIntervals == 0 {
		t.Fatal("no timestamped packet pairs")
	}
	wait("probe lease release", func() bool { return subscribers() == 0 })
	// Decode two frames, then disconnect; repeat to prove on-demand restart.
	for round := 0; round < 2; round++ {
		clientCtx, cancelClient := context.WithTimeout(ctx, 20*time.Second)
		client := exec.CommandContext(clientCtx, "ffmpeg", "-hide_banner", "-v", "error", "-rtsp_transport", "tcp", "-probesize", "32", "-analyzeduration", "0", "-threads", "1", "-flags", "low_delay", "-i", fmt.Sprintf("rtsp://%s/intercom", ports[1]), "-frames:v", "2", "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "yuv420p", "pipe:1")
		output, err := client.Output()
		cancelClient()
		if err != nil {
			t.Fatalf("round %d decode: %v", round, err)
		}
		if len(output) != 2*640*480*3/2 {
			t.Fatalf("decoded bytes = %d", len(output))
		}
		wait("MJPEG lease release", func() bool { return subscribers() == 0 })
	}
}
