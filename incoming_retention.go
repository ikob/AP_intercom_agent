// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type incomingJPEGFile struct {
	name string
	path string
	at   time.Time
}

var incomingJPEGTimestampLength = len(time.Unix(0, 0).UTC().Format(incomingJPEGTimeFmt))

func ensureIncomingJPEGDirectory(directory string) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("incoming JPEG directory must be absolute: %q", directory)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("incoming JPEG path is not a real directory: %s", directory)
	}
	return nil
}

func parseIncomingJPEGFilename(name string) (time.Time, bool) {
	if len(name) <= incomingJPEGTimestampLength+len("_.jpg") ||
		name[incomingJPEGTimestampLength] != '_' || !strings.HasSuffix(name, ".jpg") {
		return time.Time{}, false
	}
	timestamp, err := time.Parse(incomingJPEGTimeFmt, name[:incomingJPEGTimestampLength])
	if err != nil {
		return time.Time{}, false
	}
	caller := strings.TrimSuffix(name[incomingJPEGTimestampLength+1:], ".jpg")
	if caller == "" || safeIncomingCaller(caller) != caller {
		return time.Time{}, false
	}
	return timestamp, true
}

func isManagedIncomingJPEGTemp(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	body := strings.TrimPrefix(name, ".")
	separator := strings.LastIndex(body, ".jpg-")
	if separator < 0 || separator+len(".jpg-") >= len(body) {
		return false
	}
	_, managed := parseIncomingJPEGFilename(body[:separator+len(".jpg")])
	return managed
}

func pruneIncomingJPEGDirectory(
	directory string,
	maxFiles int,
	protected map[string]int,
	cleanupOrphans bool,
) (int, error) {
	if maxFiles < 1 {
		return 0, fmt.Errorf("incoming JPEG max files must be at least 1")
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect incoming JPEG directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, fmt.Errorf("incoming JPEG path is not a real directory: %s", directory)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("read incoming JPEG directory: %w", err)
	}

	removed := 0
	var candidates []incomingJPEGFile
	var errs []error
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		entryInfo, err := os.Lstat(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect %s: %w", path, err))
			continue
		}
		if !entryInfo.Mode().IsRegular() {
			continue
		}
		if cleanupOrphans && isManagedIncomingJPEGTemp(entry.Name()) {
			if err := os.Remove(path); err != nil {
				errs = append(errs, fmt.Errorf("remove orphan incoming JPEG temporary file %s: %w", path, err))
			} else {
				removed++
			}
			continue
		}
		at, managed := parseIncomingJPEGFilename(entry.Name())
		if !managed {
			continue
		}
		candidates = append(candidates, incomingJPEGFile{name: entry.Name(), path: path, at: at})
	}

	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].at.Equal(candidates[right].at) {
			return candidates[left].name < candidates[right].name
		}
		return candidates[left].at.Before(candidates[right].at)
	})

	excess := len(candidates) - maxFiles
	for _, candidate := range candidates {
		if excess <= 0 {
			break
		}
		if protected[filepath.Clean(candidate.path)] > 0 {
			continue
		}
		if err := os.Remove(candidate.path); err != nil {
			errs = append(errs, fmt.Errorf("remove retained incoming JPEG %s: %w", candidate.path, err))
			continue
		}
		removed++
		excess--
	}

	return removed, errors.Join(errs...)
}

func (a *Agent) markIncomingJPEGActive(path string) {
	path = filepath.Clean(path)
	a.incomingJPEGMu.Lock()
	if a.incomingJPEGActive == nil {
		a.incomingJPEGActive = make(map[string]int)
	}
	a.incomingJPEGActive[path]++
	a.incomingJPEGMu.Unlock()
}

func (a *Agent) finishIncomingJPEGCapture(path string, prune bool) (int, error) {
	path = filepath.Clean(path)
	a.incomingJPEGMu.Lock()
	defer a.incomingJPEGMu.Unlock()
	defer func() {
		if a.incomingJPEGActive[path] <= 1 {
			delete(a.incomingJPEGActive, path)
		} else {
			a.incomingJPEGActive[path]--
		}
	}()
	if !prune {
		return 0, nil
	}
	return pruneIncomingJPEGDirectory(
		a.cfg.IncomingJPEGDir,
		a.cfg.IncomingJPEGMaxFiles,
		a.incomingJPEGActive,
		false,
	)
}

func (a *Agent) pruneIncomingJPEGsAtStartup() (int, error) {
	a.incomingJPEGMu.Lock()
	defer a.incomingJPEGMu.Unlock()
	return pruneIncomingJPEGDirectory(
		a.cfg.IncomingJPEGDir,
		a.cfg.IncomingJPEGMaxFiles,
		a.incomingJPEGActive,
		true,
	)
}
