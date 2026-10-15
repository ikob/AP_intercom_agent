// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPSnapshotUnavailableUntilFramePublished(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()

	client := http.Client{Timeout: time.Second}
	response, err := client.Get(baseURL + "/v1/camera.jpg")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status before frame = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}

	want := testHubFrame(t, solidColor(0x22, 0x44, 0x66))
	if err := agent.frameHub.Publish(want); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get(baseURL + "/v1/camera.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("snapshot Content-Type = %q", response.Header.Get("Content-Type"))
	}
	if !bytes.Equal(got, want.Data) {
		t.Fatal("snapshot body does not match the published frame")
	}
	if response.Header.Get("ETag") == "" {
		t.Fatal("snapshot has no ETag")
	}
}

func TestHTTPMJPEGStreamsPublishedFrames(t *testing.T) {
	baseURL, cancel, agent, _, _ := startHTTPTestServer(t)
	defer cancel()
	want := testHubFrame(t, solidColor(0x99, 0x77, 0x55))
	if err := agent.frameHub.Publish(want); err != nil {
		t.Fatal(err)
	}

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/v1/camera.mjpeg", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "multipart/x-mixed-replace" {
		t.Fatalf("stream media type = %q", mediaType)
	}
	reader := multipart.NewReader(response.Body, parameters["boundary"])

	for partNumber := 1; partNumber <= 2; partNumber++ {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("read MJPEG part %d: %v", partNumber, err)
		}
		contentLength, err := strconv.Atoi(part.Header.Get("Content-Length"))
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, contentLength)
		if _, err := io.ReadFull(part, got); err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("part %d Content-Type = %q", partNumber, part.Header.Get("Content-Type"))
		}
		if !bytes.Equal(got, want.Data) {
			t.Fatalf("MJPEG part %d does not match the published frame", partNumber)
		}
	}
}

func TestHTTPMJPEGStopsWithServerContext(t *testing.T) {
	baseURL, cancelServer, _, _, _ := startHTTPTestServer(t)

	response, err := http.Get(baseURL + "/v1/camera.mjpeg")
	if err != nil {
		cancelServer()
		t.Fatal(err)
	}
	defer response.Body.Close()

	finished := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		finished <- err
	}()
	cancelServer()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("read stream after shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MJPEG stream did not stop with the server context")
	}
}

func TestHTTPCameraEndpointsRejectWrites(t *testing.T) {
	baseURL, cancel, _, _, _ := startHTTPTestServer(t)
	defer cancel()

	for _, path := range []string{"/v1/camera.jpg", "/v1/camera.mjpeg"} {
		response, err := http.Post(baseURL+path, "application/octet-stream", strings.NewReader("frame"))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want %d", path, response.StatusCode, http.StatusMethodNotAllowed)
		}
		if response.Header.Get("Allow") == "" {
			t.Fatalf("POST %s response has no Allow header", path)
		}
	}
}

func solidColor(red, green, blue uint8) testColor {
	return testColor{red: red, green: green, blue: blue}
}

type testColor struct {
	red   uint8
	green uint8
	blue  uint8
}

func (c testColor) RGBA() (uint32, uint32, uint32, uint32) {
	return uint32(c.red) * 0x101, uint32(c.green) * 0x101, uint32(c.blue) * 0x101, 0xffff
}
