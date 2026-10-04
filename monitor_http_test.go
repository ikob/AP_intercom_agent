// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type monitorHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f monitorHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testJPEG(t testing.TB, fill color.Color) []byte {
	t.Helper()
	imageData := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < imageData.Bounds().Dy(); y++ {
		for x := 0; x < imageData.Bounds().Dx(); x++ {
			imageData.Set(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, imageData, nil); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestMonitorJPEGEndpointFromSDP(t *testing.T) {
	tests := []struct {
		name    string
		sdp     string
		want    string
		wantErr string
	}{
		{
			name: "session IPv4",
			sdp: "v=0\r\n" +
				"c=IN IP4 192.0.2.25\r\n" +
				"m=audio 20000 RTP/AVP 0\r\n" +
				"m=video 8080 HTTP jpeg\r\n",
			want: "http://192.0.2.25:8080/cgi-bin/image.cgi?s=" + defaultJPEGQueryS,
		},
		{
			name: "video IPv6 override",
			sdp: "v=0\r\n" +
				"c=IN IP4 192.0.2.25\r\n" +
				"m=video 18080 HTTP jpeg\r\n" +
				"c=IN IP6 2001:db8::25\r\n",
			want: "http://[2001:db8::25]:18080/cgi-bin/image.cgi?s=" + defaultJPEGQueryS,
		},
		{
			name:    "missing video",
			sdp:     "v=0\r\nc=IN IP4 192.0.2.25\r\nm=audio 20000 RTP/AVP 0\r\n",
			wantErr: "no HTTP JPEG video",
		},
		{
			name:    "wrong protocol",
			sdp:     "v=0\r\nc=IN IP4 192.0.2.25\r\nm=video 8080 RTP/AVP 96\r\n",
			wantErr: "unsupported protocol",
		},
		{
			name:    "invalid port",
			sdp:     "v=0\r\nc=IN IP4 192.0.2.25\r\nm=video 70000 HTTP jpeg\r\n",
			wantErr: "invalid monitor video port",
		},
		{
			name:    "unspecified address",
			sdp:     "v=0\r\nc=IN IP4 0.0.0.0\r\nm=video 8080 HTTP jpeg\r\n",
			wantErr: "invalid SDP connection address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := monitorJPEGEndpointFromSDP([]byte(test.sdp), defaultJPEGQueryS)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := endpoint.String(); got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMonitorJPEGEndpointUsesConfiguredQueryS(t *testing.T) {
	const queryS = "opaque/value +?&="
	endpoint, err := monitorJPEGEndpointFromSDP([]byte(
		"v=0\r\nc=IN IP4 192.0.2.25\r\nm=video 8080 HTTP jpeg\r\n",
	), queryS)
	if err != nil {
		t.Fatal(err)
	}
	if got := endpoint.Query().Get("s"); got != queryS {
		t.Fatalf("query s = %q, want %q", got, queryS)
	}
}

func TestFetchMonitorJPEG(t *testing.T) {
	jpegData := testJPEG(t, color.RGBA{R: 20, G: 40, B: 60, A: 255})
	endpoint, err := url.Parse("http://192.0.2.25:8080/cgi-bin/image.cgi?s=" + defaultJPEGQueryS)
	if err != nil {
		t.Fatal(err)
	}
	doer := monitorHTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.Host != monitorJPEGHost {
			t.Errorf("Host = %q, want %q", request.Host, monitorJPEGHost)
		}
		if request.URL.Path != monitorJPEGPath || request.URL.Query().Get("s") != defaultJPEGQueryS {
			t.Errorf("unexpected request URL %s", request.URL.Redacted())
		}
		for header, want := range map[string]string{
			"Accept":          "*/*",
			"User-Agent":      monitorJPEGUserAgent,
			"Accept-Language": "ja",
			"Accept-Encoding": "gzip, deflate",
			"Connection":      "keep-alive",
		} {
			if got := request.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
			Body:       io.NopCloser(bytes.NewReader(jpegData)),
		}, nil
	})

	frame, err := fetchMonitorJPEG(context.Background(), doer, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Width != 8 || frame.Height != 6 || !bytes.Equal(frame.Data, jpegData) {
		t.Fatalf("unexpected frame: %dx%d, %d bytes", frame.Width, frame.Height, len(frame.Data))
	}
}

func TestFetchMonitorJPEGRejectsInvalidResponses(t *testing.T) {
	validJPEG := testJPEG(t, color.Black)
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
		wantErr     string
	}{
		{name: "status", status: http.StatusInternalServerError, contentType: "image/jpeg", body: validJPEG, wantErr: "500"},
		{name: "content type", status: http.StatusOK, contentType: "text/plain", body: validJPEG, wantErr: "Content-Type"},
		{name: "truncated", status: http.StatusOK, contentType: "image/jpeg", body: validJPEG[:len(validJPEG)/2], wantErr: "decode monitor JPEG"},
		{name: "oversize", status: http.StatusOK, contentType: "image/jpeg", body: make([]byte, monitorJPEGMaxBytes+1), wantErr: "exceeds"},
	}
	endpoint, _ := url.Parse("http://192.0.2.25:8080" + monitorJPEGPath)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doer := monitorHTTPDoerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					Status:     fmt.Sprintf("%d test", test.status),
					StatusCode: test.status,
					Header:     http.Header{"Content-Type": []string{test.contentType}},
					Body:       io.NopCloser(bytes.NewReader(test.body)),
				}, nil
			})
			_, err := fetchMonitorJPEG(context.Background(), doer, endpoint)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestFetchMonitorJPEGAcceptsIFBOXResponseFraming(t *testing.T) {
	jpegData := testJPEG(t, color.White)
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "HTTP/1.1 200 OK\r\n")
	fmt.Fprintf(&raw, "Content-Type: image/jpeg\r\n")
	fmt.Fprintf(&raw, "Transfer-Encoding: chunked\r\n")
	fmt.Fprintf(&raw, "Content-Length: %d\r\n", len(jpegData))
	fmt.Fprintf(&raw, "Connection: close\r\n")
	fmt.Fprintf(&raw, "connection: Keep-Alive\r\n\r\n")
	fmt.Fprintf(&raw, "%x\r\n", len(jpegData))
	raw.Write(jpegData)
	raw.WriteString("\r\n0\r\n\r\n")

	request, _ := http.NewRequest(http.MethodGet, "http://192.0.2.25/", nil)
	response, err := http.ReadResponse(bufio.NewReader(&raw), request)
	if err != nil {
		t.Fatal(err)
	}
	doer := monitorHTTPDoerFunc(func(*http.Request) (*http.Response, error) {
		return response, nil
	})
	endpoint, _ := url.Parse("http://192.0.2.25:8080" + monitorJPEGPath)
	frame, err := fetchMonitorJPEG(context.Background(), doer, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame.Data, jpegData) {
		t.Fatalf("decoded %d bytes, want %d", len(frame.Data), len(jpegData))
	}
}

func TestWriteMonitorJPEGAtomic(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "frame.jpg")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMonitorJPEGAtomic(output, []byte("new frame")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new frame" {
		t.Fatalf("output = %q", got)
	}

	symlink := filepath.Join(directory, "frame-link.jpg")
	if err := os.Symlink(output, symlink); err != nil {
		t.Fatal(err)
	}
	if err := writeMonitorJPEGAtomic(symlink, []byte("bad")); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}
