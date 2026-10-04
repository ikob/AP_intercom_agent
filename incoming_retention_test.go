// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Katsushi Kobayashi

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseIncomingJPEGFilename(t *testing.T) {
	want := time.Date(2026, time.October, 4, 21, 41, 23, 123456789, time.FixedZone("JST", 9*60*60))
	name := incomingJPEGFilename(want, "interphone0")
	got, ok := parseIncomingJPEGFilename(name)
	if !ok || !got.Equal(want) {
		t.Fatalf("parse %q = %v, %v; want %v, true", name, got, ok, want)
	}
	for _, invalid := range []string{
		"frame.jpg",
		"20261004T214123.123456789+0900_.jpg",
		"20261004T214123.123456789+0900_../door.jpg",
		"20261004T214123+0900_interphone0.jpg",
	} {
		if _, ok := parseIncomingJPEGFilename(invalid); ok {
			t.Fatalf("accepted unmanaged filename %q", invalid)
		}
	}
}

func TestPruneIncomingJPEGDirectory(t *testing.T) {
	directory := t.TempDir()
	zone := time.FixedZone("JST", 9*60*60)
	var paths []string
	for index := 0; index < 4; index++ {
		path := filepath.Join(directory, incomingJPEGFilename(
			time.Date(2026, time.October, 4, 12, 0, index, 0, zone),
			"interphone0",
		))
		if err := os.WriteFile(path, []byte{byte(index)}, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	unrelated := filepath.Join(directory, "keep-me.jpg")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdirectory := filepath.Join(directory, incomingJPEGFilename(time.Now(), "directory"))
	if err := os.Mkdir(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, incomingJPEGFilename(time.Now().Add(time.Second), "symlink"))
	if err := os.Symlink(unrelated, symlink); err != nil {
		t.Fatal(err)
	}

	removed, err := pruneIncomingJPEGDirectory(directory, 2, map[string]int{paths[0]: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range []string{paths[0], paths[3], unrelated, subdirectory, symlink} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("protected/unmanaged path %s was removed: %v", path, err)
		}
	}
	for _, path := range []string{paths[1], paths[2]} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("old path %s still exists: %v", path, err)
		}
	}
}

func TestPruneIncomingJPEGDirectoryCleansManagedOrphans(t *testing.T) {
	directory := t.TempDir()
	base := incomingJPEGFilename(time.Now(), "interphone0")
	orphan := filepath.Join(directory, "."+base+"-12345")
	unrelated := filepath.Join(directory, ".other.jpg-12345")
	for _, path := range []string{orphan, unrelated} {
		if err := os.WriteFile(path, []byte("temporary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := pruneIncomingJPEGDirectory(directory, 100, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("managed orphan still exists: %v", err)
	}
	if _, err := os.Lstat(unrelated); err != nil {
		t.Fatalf("unrelated temporary file was removed: %v", err)
	}
}

func TestEnsureIncomingJPEGDirectoryRejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureIncomingJPEGDirectory(link); err == nil {
		t.Fatal("expected symlink directory to be rejected")
	}
}

func TestPruneIncomingJPEGDirectoryRejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneIncomingJPEGDirectory(link, 100, nil, true); err == nil {
		t.Fatal("expected symlink directory to be rejected")
	}
}
