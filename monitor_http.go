// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	monitorJPEGPath       = "/cgi-bin/image.cgi"
	defaultJPEGQueryS     = "cd188d8e518cb12ad7a644f23928f64f"
	monitorJPEGMaxBytes   = 2 << 20
	monitorJPEGHost       = "ifbox"
	monitorJPEGUserAgent  = "AIPHONE/1.0 iPhone18,1(intercom)"
	monitorJPEGReqTimeout = 3 * time.Second
)

type monitorJPEGFrame struct {
	Data     []byte
	Width    int
	Height   int
	SHA256   [sha256.Size]byte
	Duration time.Duration
}

type monitorHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func (a *Agent) monitorJPEGEndpoint() (*url.URL, error) {
	a.monitorMu.Lock()
	if a.monitorState != monitorConfirmed || a.monitorDialog == nil {
		state := a.monitorState
		a.monitorMu.Unlock()
		return nil, fmt.Errorf("monitor JPEG endpoint unavailable in state %d", state)
	}
	response := a.monitorDialog.InviteResponse
	if response == nil {
		a.monitorMu.Unlock()
		return nil, errors.New("monitor answer is unavailable")
	}
	answer := append([]byte(nil), response.Body()...)
	a.monitorMu.Unlock()

	return monitorJPEGEndpointFromSDP(answer, a.cfg.JPEGQueryS)
}

func monitorJPEGEndpointFromSDP(answer []byte, queryS string) (*url.URL, error) {
	var sessionHost string
	var videoHost string
	var videoPort int
	foundVideo := false
	seenMedia := false
	inVideo := false

	normalized := strings.ReplaceAll(string(answer), "\r\n", "\n")
	for _, rawLine := range strings.Split(normalized, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "m=") {
			seenMedia = true
			inVideo = false
			fields := strings.Fields(line)
			if len(fields) < 4 || !strings.EqualFold(strings.TrimPrefix(fields[0], "m="), "video") {
				continue
			}
			if foundVideo {
				continue
			}
			if !strings.EqualFold(fields[2], "HTTP") {
				return nil, fmt.Errorf("monitor video uses unsupported protocol %q", fields[2])
			}
			jpegFormat := false
			for _, format := range fields[3:] {
				if strings.EqualFold(format, "jpeg") {
					jpegFormat = true
					break
				}
			}
			if !jpegFormat {
				return nil, fmt.Errorf("monitor video has no JPEG format: %q", line)
			}
			portText := strings.SplitN(fields[1], "/", 2)[0]
			port, err := strconv.Atoi(portText)
			if err != nil || port < 1 || port > 65535 {
				return nil, fmt.Errorf("invalid monitor video port %q", fields[1])
			}
			videoPort = port
			foundVideo = true
			inVideo = true
			continue
		}

		if strings.HasPrefix(line, "c=") {
			host, err := sdpConnectionHost(line)
			if err != nil {
				return nil, err
			}
			switch {
			case !seenMedia:
				sessionHost = host
			case inVideo:
				videoHost = host
			}
		}
	}

	if !foundVideo {
		return nil, errors.New("monitor answer has no HTTP JPEG video media")
	}
	if videoHost == "" {
		videoHost = sessionHost
	}
	if videoHost == "" {
		return nil, errors.New("monitor answer has no connection address for video")
	}

	query := url.Values{}
	query.Set("s", queryS)
	return &url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(videoHost, strconv.Itoa(videoPort)),
		Path:     monitorJPEGPath,
		RawQuery: query.Encode(),
	}, nil
}

func sdpConnectionHost(line string) (string, error) {
	fields := strings.Fields(strings.TrimPrefix(line, "c="))
	if len(fields) != 3 || !strings.EqualFold(fields[0], "IN") {
		return "", fmt.Errorf("invalid SDP connection line %q", line)
	}
	if !strings.EqualFold(fields[1], "IP4") && !strings.EqualFold(fields[1], "IP6") {
		return "", fmt.Errorf("unsupported SDP address type %q", fields[1])
	}
	host := strings.SplitN(fields[2], "/", 2)[0]
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return "", fmt.Errorf("invalid SDP connection address %q", host)
	}
	return host, nil
}

func newMonitorHTTPClient() (*http.Client, *http.Transport) {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: monitorJPEGReqTimeout,
		DisableCompression:    true,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("monitor JPEG redirect is not allowed")
		},
	}
	return client, transport
}

func fetchMonitorJPEG(ctx context.Context, client monitorHTTPDoer, endpoint *url.URL) (monitorJPEGFrame, error) {
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("create monitor JPEG request: %w", err)
	}
	request.Host = monitorJPEGHost
	request.Header.Set("Accept", "*/*")
	request.Header.Set("User-Agent", monitorJPEGUserAgent)
	request.Header.Set("Accept-Language", "ja")
	request.Header.Set("Accept-Encoding", "gzip, deflate")
	request.Header.Set("Connection", "keep-alive")

	response, err := client.Do(request)
	if err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("GET monitor JPEG: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return monitorJPEGFrame{}, fmt.Errorf("GET monitor JPEG: unexpected HTTP status %s", response.Status)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "image/jpeg") {
		return monitorJPEGFrame{}, fmt.Errorf("GET monitor JPEG: unexpected Content-Type %q", response.Header.Get("Content-Type"))
	}

	reader := io.Reader(response.Body)
	var decoder io.Closer
	switch encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))); encoding {
	case "", "identity":
	case "gzip":
		gzipReader, err := gzip.NewReader(response.Body)
		if err != nil {
			return monitorJPEGFrame{}, fmt.Errorf("open gzip monitor JPEG: %w", err)
		}
		reader = gzipReader
		decoder = gzipReader
	case "deflate":
		zlibReader, err := zlib.NewReader(response.Body)
		if err != nil {
			return monitorJPEGFrame{}, fmt.Errorf("open deflate monitor JPEG: %w", err)
		}
		reader = zlibReader
		decoder = zlibReader
	default:
		return monitorJPEGFrame{}, fmt.Errorf("GET monitor JPEG: unsupported Content-Encoding %q", encoding)
	}
	if decoder != nil {
		defer decoder.Close()
	}

	data, err := io.ReadAll(io.LimitReader(reader, monitorJPEGMaxBytes+1))
	if err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("read monitor JPEG: %w", err)
	}
	if len(data) > monitorJPEGMaxBytes {
		return monitorJPEGFrame{}, fmt.Errorf("monitor JPEG exceeds %d bytes", monitorJPEGMaxBytes)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("decode monitor JPEG: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 {
		return monitorJPEGFrame{}, fmt.Errorf("monitor JPEG has invalid dimensions %dx%d", config.Width, config.Height)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return monitorJPEGFrame{}, fmt.Errorf("decode monitor JPEG: %w", err)
	}

	return monitorJPEGFrame{
		Data:     data,
		Width:    config.Width,
		Height:   config.Height,
		SHA256:   sha256.Sum256(data),
		Duration: time.Since(started),
	}, nil
}

func writeMonitorJPEGAtomic(path string, data []byte) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("monitor JPEG output must be an absolute path: %q", path)
	}
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("stat monitor JPEG output directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("monitor JPEG output parent is not a directory: %s", directory)
	}
	if existing, err := os.Lstat(path); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return fmt.Errorf("monitor JPEG output is not a regular file: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect monitor JPEG output: %w", err)
	}

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary monitor JPEG: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set monitor JPEG permissions: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write monitor JPEG: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync monitor JPEG: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close monitor JPEG: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace monitor JPEG: %w", err)
	}
	keep = true
	return nil
}

func (a *Agent) pollMonitorJPEG(ctx context.Context, output string, interval time.Duration) (frames int, changed bool, err error) {
	endpoint, err := a.monitorJPEGEndpoint()
	if err != nil {
		return 0, false, err
	}
	return pollMonitorJPEGEndpoint(ctx, endpoint, output, interval)
}

func pollMonitorJPEGEndpoint(ctx context.Context, endpoint *url.URL, output string, interval time.Duration) (frames int, changed bool, err error) {
	client, transport := newMonitorHTTPClient()
	defer transport.CloseIdleConnections()

	var baseline [sha256.Size]byte
	for {
		if err := ctx.Err(); err != nil {
			if frames == 0 {
				return 0, false, fmt.Errorf("no monitor JPEG received: %w", err)
			}
			return frames, changed, nil
		}
		requestStarted := time.Now()
		requestCtx, cancel := context.WithTimeout(ctx, monitorJPEGReqTimeout)
		frame, err := fetchMonitorJPEG(requestCtx, client, endpoint)
		cancel()
		if err != nil {
			if ctx.Err() != nil && frames > 0 {
				return frames, changed, nil
			}
			return frames, changed, err
		}
		if err := writeMonitorJPEGAtomic(output, frame.Data); err != nil {
			return frames, changed, err
		}
		frames++
		if frames == 1 {
			baseline = frame.SHA256
		} else if frame.SHA256 != baseline {
			changed = true
		}
		slog.Info("Monitor JPEG received",
			"frame", frames,
			"bytes", len(frame.Data),
			"width", frame.Width,
			"height", frame.Height,
			"sha256", fmt.Sprintf("%x", frame.SHA256[:8]),
			"changed", changed,
			"duration", frame.Duration,
			"output", output,
		)

		wait := interval - time.Since(requestStarted)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return frames, changed, nil
		}
	}
}
