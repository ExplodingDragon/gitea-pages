// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifact

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxPathBytes = 1024
	maxPathDepth = 32
	maxLinkDepth = 32
)

type archiveEntryKind uint8

const (
	archiveEntryRegular archiveEntryKind = iota
	archiveEntryDirectory
	archiveEntrySymlink
)

type archiveEntry struct {
	kind       archiveEntryKind
	temp, link string
}

// Limits are the content bounds configured by Gitea for new deployments.
type Limits struct {
	MaxTarGzBytes int64 `json:"max_tar_gz_bytes"`
	MaxSiteBytes  int64 `json:"max_site_bytes"`
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxEntries    int   `json:"max_entries"`
}

// ValidDigest reports whether digest is a lowercase SHA-256 value.
func ValidDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Validate checks whether limits can be applied without integer overflow.
func (l Limits) Validate() error {
	_, err := l.maxTarBytes()
	return err
}

// Extract validates and expands one site into a new target directory.
func Extract(ctx context.Context, src *os.File, target string, limits Limits) (retErr error) {
	maxTarBytes, err := limits.maxTarBytes()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if info, err := src.Stat(); err != nil {
		return err
	} else if info.Size() > limits.MaxTarGzBytes {
		return fmt.Errorf("compressed archive exceeds %d bytes", limits.MaxTarGzBytes)
	}
	buffered := bufio.NewReader(src)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() {
		if err := gz.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	// A gzip stream may contain concatenated members. Pages accepts exactly one
	// member so that bytes appended after a valid archive cannot be hidden from
	// the validation below.
	gz.Multistream(false)
	tarData := &boundedArchiveReader{ctx: ctx, r: gz, limit: maxTarBytes + 1}
	tr := tar.NewReader(tarData)
	if _, err := os.Lstat(target); err == nil {
		return errors.New("archive target already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(target, ".archive-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tempDir); retErr == nil && err != nil {
			retErr = fmt.Errorf("remove archive temporary directory: %w", err)
		}
	}()
	entries, hasRegularFile, err := readArchiveEntries(tr, tempDir, limits)
	if err != nil {
		return err
	}
	if err := validateArchiveEnd(buffered, tarData, maxTarBytes); err != nil {
		return err
	}
	if !hasRegularFile {
		return errors.New("site archive has no regular file")
	}
	if err := validateArchiveTree(entries); err != nil {
		return err
	}
	return installArchive(target, entries)
}

func readArchiveEntries(tr *tar.Reader, tempDir string, limits Limits) (map[string]archiveEntry, bool, error) {
	entries := make(map[string]archiveEntry)
	canonical := make(map[string]string)
	var total int64
	count := 0
	hasRegularFile := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("tar: %w", err)
		}
		count++
		if count > limits.MaxEntries {
			return nil, false, errors.New("too many archive entries")
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return nil, false, err
		}
		if name == "" {
			continue
		}
		if strings.HasSuffix(h.Name, "/") && h.Typeflag != tar.TypeDir {
			return nil, false, fmt.Errorf("non-directory archive entry %q has a trailing slash", name)
		}
		if name == ".gitea-pages-probe" || strings.HasPrefix(name, ".gitea-pages-probe/") {
			return nil, false, fmt.Errorf("reserved Pages path %q", name)
		}
		key := canonicalArchivePath(name)
		if old, ok := canonical[key]; ok {
			return nil, false, fmt.Errorf("archive path %q conflicts with %q", name, old)
		}
		canonical[key] = name
		if _, ok := entries[name]; ok {
			return nil, false, fmt.Errorf("duplicate archive path %q", name)
		}
		var kind archiveEntryKind
		switch h.Typeflag {
		case tar.TypeReg:
			kind = archiveEntryRegular
			if h.Size < 0 || h.Size > limits.MaxFileBytes {
				return nil, false, fmt.Errorf("file %q is too large", name)
			}
		case tar.TypeDir:
			kind = archiveEntryDirectory
			if h.Size != 0 {
				return nil, false, fmt.Errorf("directory %q has data", name)
			}
		case tar.TypeSymlink:
			kind = archiveEntrySymlink
			if h.Size != 0 {
				return nil, false, fmt.Errorf("symlink %q has data", name)
			}
			if err := validateArchiveLinkTarget(h.Linkname); err != nil {
				return nil, false, fmt.Errorf("link %q: %w", name, err)
			}
		default:
			return nil, false, fmt.Errorf("unsupported tar entry %q", name)
		}
		entry := archiveEntry{kind: kind, link: h.Linkname}
		if kind == archiveEntryRegular {
			hasRegularFile = true
			if h.Size > limits.MaxSiteBytes-total {
				return nil, false, errors.New("site is too large")
			}
			total += h.Size
			tmp, e := os.CreateTemp(tempDir, "file-")
			if e != nil {
				return nil, false, e
			}
			_, copyErr := io.CopyN(tmp, tr, h.Size)
			closeErr := tmp.Close()
			if copyErr != nil {
				return nil, false, fmt.Errorf("read file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return nil, false, closeErr
			}
			entry.temp = tmp.Name()
		}
		entries[name] = entry
	}
	return entries, hasRegularFile, nil
}

func validateArchiveEnd(buffered *bufio.Reader, tarData *boundedArchiveReader, maxTarBytes int64) error {
	// tar.Reader stops after the two zero records that terminate a TAR. Read
	// the rest of the member explicitly so an oversized or non-zero TAR tail is
	// rejected, while also forcing gzip to verify its checksum and footer.
	var tail [32 * 1024]byte
	for {
		n, readErr := tarData.Read(tail[:])
		for _, b := range tail[:n] {
			if b != 0 {
				return errors.New("non-zero data after TAR end")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("TAR data: %w", readErr)
		}
	}
	if tarData.n > maxTarBytes {
		return fmt.Errorf("TAR data exceeds %d bytes", maxTarBytes)
	}
	if _, err := buffered.Peek(1); err == nil {
		return errors.New("gzip stream has more than one member or trailing data")
	} else if err != io.EOF {
		return fmt.Errorf("read after gzip stream: %w", err)
	}
	return nil
}

func installArchive(target string, entries map[string]archiveEntry) error {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := entries[n]
		p := filepath.Join(target, filepath.FromSlash(n))
		switch e.kind {
		case archiveEntryDirectory:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case archiveEntryRegular, archiveEntrySymlink:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if e.kind == archiveEntrySymlink {
				if err := os.Symlink(e.link, p); err != nil {
					return err
				}
				continue
			}
			if err := os.Rename(e.temp, p); err != nil {
				return err
			}
			// Archive modes never make published files executable.
			if err := os.Chmod(p, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l Limits) maxTarBytes() (int64, error) {
	const archiveOverhead = int64(1024 * 1024)
	if l.MaxTarGzBytes <= 0 || l.MaxTarGzBytes == math.MaxInt64 || l.MaxSiteBytes <= 0 || l.MaxFileBytes <= 0 || l.MaxFileBytes > l.MaxSiteBytes || l.MaxEntries <= 0 ||
		l.MaxSiteBytes > math.MaxInt64-archiveOverhead || int64(l.MaxEntries) > (math.MaxInt64-l.MaxSiteBytes-archiveOverhead)/4096 {
		return 0, errors.New("invalid archive limits from Gitea")
	}
	return l.MaxSiteBytes + int64(l.MaxEntries)*4096 + archiveOverhead, nil
}

func archivePath(raw string) (string, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\\') || len(raw) > maxPathBytes {
		return "", errors.New("invalid archive path")
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || windowsArchiveVolume(raw) {
		return "", fmt.Errorf("absolute archive path %q", raw)
	}
	trimmed := strings.TrimRight(raw, "/")
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	parts := strings.Split(trimmed, "/")
	for len(parts) > 0 && parts[0] == "." {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "", nil
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || hasArchiveControl(part) {
			return "", fmt.Errorf("invalid archive path %q", raw)
		}
	}
	if len(parts) > maxPathDepth {
		return "", fmt.Errorf("archive path %q is too deep", raw)
	}
	name := strings.Join(parts, "/")
	if len(name) > maxPathBytes {
		return "", fmt.Errorf("archive path %q is too long", raw)
	}
	return name, nil
}

func validateArchiveLinkTarget(raw string) error {
	if raw == "" || !utf8.ValidString(raw) || len(raw) > maxPathBytes || strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\\') || hasArchiveControl(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || windowsArchiveVolume(raw) {
		return errors.New("invalid relative target")
	}
	return nil
}

func windowsArchiveVolume(raw string) bool {
	return len(raw) >= 2 && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')) && raw[1] == ':'
}

func hasArchiveControl(raw string) bool {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func canonicalArchivePath(name string) string {
	return cases.Fold().String(norm.NFC.String(name))
}

type boundedArchiveReader struct {
	ctx      context.Context
	r        io.Reader
	limit, n int64
}

func (r *boundedArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.n >= r.limit {
		return 0, fmt.Errorf("TAR data exceeds %d bytes", r.limit)
	}
	if int64(len(p)) > r.limit-r.n {
		p = p[:r.limit-r.n]
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func validateArchiveTree(entries map[string]archiveEntry) error {
	for name, entry := range entries {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			if parentEntry, ok := entries[parent]; ok && parentEntry.kind != archiveEntryDirectory {
				return fmt.Errorf("path %q has non-directory parent %q", name, parent)
			}
		}
		if entry.kind == archiveEntrySymlink {
			if err := resolveArchiveLink(name, entries); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveArchiveLink(name string, entries map[string]archiveEntry) error {
	pending := make([]string, 0, maxPathDepth)
	if base := path.Dir(name); base != "." {
		pending = append(pending, strings.Split(base, "/")...)
	}
	pending = append(pending, strings.Split(entries[name].link, "/")...)
	resolved := make([]string, 0, maxPathDepth)
	seen := make(map[string]bool)
	depth := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("symlink %q escapes site", name)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		default:
			resolved = append(resolved, part)
		}
		if len(resolved) > maxPathDepth {
			return fmt.Errorf("symlink %q is too deep", name)
		}
		current := strings.Join(resolved, "/")
		entry, ok := entries[current]
		if !ok {
			continue
		}
		if entry.kind == archiveEntryRegular && len(pending) > 0 {
			return fmt.Errorf("symlink %q enters a file", name)
		}
		if entry.kind != archiveEntrySymlink {
			continue
		}
		depth++
		if depth > maxLinkDepth || seen[current] {
			return fmt.Errorf("symlink %q loops or exceeds depth", name)
		}
		seen[current] = true
		resolved = resolved[:len(resolved)-1]
		pending = append(strings.Split(entry.link, "/"), pending...)
	}
	return nil
}
