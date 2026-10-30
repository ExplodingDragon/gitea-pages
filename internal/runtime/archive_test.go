// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testTarEntry struct {
	name string
	kind byte
	body string
	link string
	mode int64
	size int64
}

func testArchiveLimits() archiveLimits {
	return archiveLimits{MaxTarGzBytes: 100 * 1024 * 1024, MaxSiteBytes: 512 * 1024 * 1024, MaxFileBytes: 100 * 1024 * 1024, MaxEntries: 10_000}
}

func makeArchive(t *testing.T, entries ...testTarEntry) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "site-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode}
		if e.kind == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.kind == tar.TypeSymlink {
			h.Size = e.size
		}
		if e.kind == tar.TypeSymlink {
			h.Linkname = e.link
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExtractTarGzSanitizesFilesAndAcceptsMissingInRootSymlink(t *testing.T) {
	f := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "ok", mode: 0o755}, testTarEntry{name: "assets/logo", kind: tar.TypeSymlink, link: "../missing/logo"})
	defer func() { _ = f.Close() }()
	target := filepath.Join(t.TempDir(), "site")
	if err := extractTarGz(t.Context(), f, target, testArchiveLimits()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(target, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	link, err := os.Readlink(filepath.Join(target, "assets/logo"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "../missing/logo" {
		t.Fatalf("link = %q", link)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "assets" || entries[1].Name() != "index.html" {
		t.Fatalf("extracted entries = %v", entries)
	}
}

func TestExtractTarGzHonorsDeploymentCancellation(t *testing.T) {
	f := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "ok"})
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := extractTarGz(ctx, f, filepath.Join(t.TempDir(), "site"), testArchiveLimits()); err != context.Canceled {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestExtractTarGzUsesSynchronizedLimits(t *testing.T) {
	f := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "too large"})
	defer func() { _ = f.Close() }()
	limits := testArchiveLimits()
	limits.MaxFileBytes = 4
	if err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), limits); err == nil {
		t.Fatal("archive exceeded the synchronized file limit")
	}
}

func TestExtractTarGzRejectsEscapeAndReservedPath(t *testing.T) {
	for _, name := range []string{"../index.html", ".gitea-pages-probe/evil"} {
		t.Run(name, func(t *testing.T) {
			f := makeArchive(t, testTarEntry{name: name, kind: tar.TypeReg, body: "bad"})
			defer func() { _ = f.Close() }()
			err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), testArchiveLimits())
			if err == nil {
				t.Fatal("expected archive rejection")
			}
		})
	}
	f := makeArchive(t,
		testTarEntry{name: "outside", kind: tar.TypeSymlink, link: "../../outside"},
		testTarEntry{name: "outside/file.txt", kind: tar.TypeReg, body: "bad"},
	)
	defer func() { _ = f.Close() }()
	if err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), testArchiveLimits()); err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestExtractTarGzRejectsUnsafeArchiveDetails(t *testing.T) {
	tests := []struct {
		name    string
		entries []testTarEntry
	}{
		{name: "case collision", entries: []testTarEntry{
			{name: "index.html", kind: tar.TypeReg, body: "ok"},
			{name: "INDEX.HTML", kind: tar.TypeReg, body: "same"},
		}},
		{name: "unicode collision", entries: []testTarEntry{
			{name: "e\u0301.html", kind: tar.TypeReg, body: "one"},
			{name: "é.html", kind: tar.TypeReg, body: "two"},
		}},
		{name: "path depth", entries: []testTarEntry{
			{name: strings.Repeat("a/", maxPathDepth) + "index.html", kind: tar.TypeReg, body: "deep"},
		}},
		{name: "control path", entries: []testTarEntry{
			{name: "bad\nname", kind: tar.TypeReg, body: "bad"},
		}},
		{name: "windows volume", entries: []testTarEntry{
			{name: "C:evil.html", kind: tar.TypeReg, body: "bad"},
		}},
		{name: "symlink control target", entries: []testTarEntry{
			{name: "index.html", kind: tar.TypeReg, body: "ok"},
			{name: "asset", kind: tar.TypeSymlink, link: "bad\nname"},
		}},
		{name: "symlink windows volume", entries: []testTarEntry{
			{name: "index.html", kind: tar.TypeReg, body: "ok"},
			{name: "asset", kind: tar.TypeSymlink, link: "C:secret"},
		}},
		{name: "symlink data", entries: []testTarEntry{
			{name: "index.html", kind: tar.TypeReg, body: "ok"},
			{name: "asset", kind: tar.TypeSymlink, link: "index.html", size: 1},
		}},
		{name: "file parent", entries: []testTarEntry{
			{name: "assets", kind: tar.TypeReg, body: "file"},
			{name: "assets/logo", kind: tar.TypeReg, body: "bad"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := makeArchive(t, tt.entries...)
			defer func() { _ = f.Close() }()
			if err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), testArchiveLimits()); err == nil {
				t.Fatal("expected archive rejection")
			}
		})
	}
}

func TestExtractTarGzRejectsConcatenatedGzipMembers(t *testing.T) {
	first := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "one"})
	defer func() { _ = first.Close() }()
	firstData, err := io.ReadAll(first)
	if err != nil {
		t.Fatal(err)
	}
	second := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "two"})
	defer func() { _ = second.Close() }()
	secondData, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	joined := append([]byte(nil), firstData...)
	joined = append(joined, secondData...)
	archive := filepath.Join(t.TempDir(), "joined.tar.gz")
	if err := os.WriteFile(archive, joined, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), testArchiveLimits()); err == nil {
		t.Fatal("expected concatenated gzip rejection")
	}
}

func TestExtractTarGzRejectsNonZeroTarTail(t *testing.T) {
	archive := makeArchive(t, testTarEntry{name: "index.html", kind: tar.TypeReg, body: "one"})
	defer func() { _ = archive.Close() }()
	data, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	var withTail bytes.Buffer
	gz := gzip.NewWriter(&withTail)
	// Copy the decompressed TAR, then add a byte after its two end blocks.
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gz.Write(append(decompressed, 1)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tail.tar.gz")
	if err := os.WriteFile(path, withTail.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := extractTarGz(t.Context(), f, filepath.Join(t.TempDir(), "site"), testArchiveLimits()); err == nil {
		t.Fatal("expected non-zero TAR tail rejection")
	}
}

func TestBoundedArchiveReaderRejectsOversizedTarData(t *testing.T) {
	r := &boundedArchiveReader{r: strings.NewReader("abcdef"), limit: 3}
	buf := make([]byte, 3)
	if n, err := r.Read(buf); n != 3 || err != nil {
		t.Fatalf("first read = %d, %v", n, err)
	}
	if _, err := r.Read(buf[:1]); err == nil {
		t.Fatal("expected TAR limit error")
	}
}
