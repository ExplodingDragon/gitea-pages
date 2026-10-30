// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

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

type archiveEntry struct {
	name string
	kind byte
	temp string
	link string
}

// ExtractTarGz validates and expands one site into a new target directory.
func ExtractTarGz(ctx context.Context, src *os.File, target string, repoID int64, digest string, limits ArchiveLimits) (retErr error) {
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
	entries := make(map[string]archiveEntry)
	canonical := make(map[string]string)
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
	var total int64
	count := 0
	regularFiles := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		count++
		if count > limits.MaxEntries {
			return errors.New("too many archive entries")
		}
		name, err := archivePath(h.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if strings.HasSuffix(h.Name, "/") && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("non-directory archive entry %q has a trailing slash", name)
		}
		if name == ".gitea-pages-probe" || strings.HasPrefix(name, ".gitea-pages-probe/") {
			return fmt.Errorf("reserved Pages path %q", name)
		}
		key := canonicalArchivePath(name)
		if old, ok := canonical[key]; ok {
			return fmt.Errorf("archive path %q conflicts with %q", name, old)
		}
		canonical[key] = name
		if _, ok := entries[name]; ok {
			return fmt.Errorf("duplicate archive path %q", name)
		}
		var kind byte
		switch h.Typeflag {
		case tar.TypeReg:
			kind = 'f'
			if h.Size < 0 || h.Size > limits.MaxFileBytes {
				return fmt.Errorf("file %q is too large", name)
			}
		case tar.TypeDir:
			kind = 'd'
			if h.Size != 0 {
				return fmt.Errorf("directory %q has data", name)
			}
		case tar.TypeSymlink:
			kind = 'l'
			if h.Size != 0 {
				return fmt.Errorf("symlink %q has data", name)
			}
			if err := validateLink(h.Linkname); err != nil {
				return fmt.Errorf("link %q: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry %q", name)
		}
		if kind == 'f' {
			regularFiles++
			if h.Size > limits.MaxSiteBytes-total {
				return errors.New("site is too large")
			}
			total += h.Size
			tmp, e := os.CreateTemp(tempDir, "file-")
			if e != nil {
				return e
			}
			_, copyErr := io.CopyN(tmp, tr, h.Size)
			closeErr := tmp.Close()
			if copyErr != nil {
				return fmt.Errorf("read file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
			entries[name] = archiveEntry{name: name, kind: kind, temp: tmp.Name()}
		} else {
			entries[name] = archiveEntry{name: name, kind: kind, link: h.Linkname}
		}
	}
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
	if len(entries) == 0 {
		return errors.New("empty site archive")
	}
	if regularFiles == 0 {
		return errors.New("site archive has no regular file")
	}
	if err := validateArchive(entries); err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := entries[n]
		if e.kind != 'd' {
			continue
		}
		if err := os.MkdirAll(filepath.Join(target, filepath.FromSlash(n)), 0o755); err != nil {
			return err
		}
	}
	for _, n := range names {
		e := entries[n]
		if e.kind != 'f' {
			continue
		}
		p := filepath.Join(target, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(e.temp, 0o600); err != nil {
			return err
		}
		if err := os.Rename(e.temp, p); err != nil {
			return err
		}
		// Published files are always data files. Archive mode bits are not
		// allowed to make a served file executable.
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
	}
	for _, n := range names {
		e := entries[n]
		if e.kind != 'l' {
			continue
		}
		p := filepath.Join(target, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(e.link, p); err != nil {
			return err
		}
	}
	probe := filepath.Join(target, ".gitea-pages-probe")
	if err := os.WriteFile(probe, []byte(fmt.Sprintf("%d %s\n", repoID, digest)), 0o644); err != nil {
		return err
	}
	return nil
}

func (l ArchiveLimits) maxTarBytes() (int64, error) {
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
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || hasArchiveControl(p) {
			return "", fmt.Errorf("invalid archive path %q", raw)
		}
		out = append(out, p)
	}
	if len(out) > maxPathDepth {
		return "", fmt.Errorf("archive path %q is too deep", raw)
	}
	name := strings.Join(out, "/")
	if len(name) > maxPathBytes {
		return "", fmt.Errorf("archive path %q is too long", raw)
	}
	return name, nil
}

func validateLink(raw string) error {
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
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
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

func validateArchive(es map[string]archiveEntry) error {
	for n, e := range es {
		if e.kind == 'l' {
			if _, err := resolveArchiveLink(n, es, 0); err != nil {
				return err
			}
		}
		parent := path.Dir(n)
		for parent != "." {
			if x, ok := es[parent]; ok && x.kind != 'd' {
				return fmt.Errorf("path %q has non-directory parent %q", n, parent)
			}
			parent = path.Dir(parent)
		}
	}
	return nil
}

func resolveArchiveLink(name string, es map[string]archiveEntry, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", errors.New("symlink depth exceeded")
	}
	e := es[name]
	target := e.link
	base := path.Dir(name)
	cur := path.Clean(path.Join(base, target))
	if cur == "." || strings.HasPrefix(cur, "../") || cur == ".." {
		return "", fmt.Errorf("symlink %q escapes site", name)
	}
	parts := strings.Split(cur, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if x, ok := es[prefix]; ok && x.kind == 'l' {
			resolved, err := resolveArchiveLink(prefix, es, depth+1)
			if err != nil {
				return "", err
			}
			remainder := strings.Join(parts[i:], "/")
			if remainder == "" {
				return resolved, nil
			}
			return resolveArchivePath(resolved+"/"+remainder, es, depth+1)
		}
	}
	return cur, nil
}

func resolveArchivePath(name string, es map[string]archiveEntry, depth int) (string, error) {
	if depth > maxLinkDepth {
		return "", errors.New("symlink depth exceeded")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("symlink escapes site")
	}
	parts := strings.Split(clean, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if x, ok := es[prefix]; ok && x.kind == 'l' {
			resolved, err := resolveArchiveLink(prefix, es, depth+1)
			if err != nil {
				return "", err
			}
			remainder := strings.Join(parts[i:], "/")
			if remainder == "" {
				return resolved, nil
			}
			return resolveArchivePath(resolved+"/"+remainder, es, depth+1)
		}
	}
	return clean, nil
}
