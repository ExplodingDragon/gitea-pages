package page

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The platform resources are deliberately embedded. A Pages instance can
// therefore answer its own status and error pages while its content store is
// empty or Gitea is unavailable.
//
//go:embed resources
var embeddedResources embed.FS

const (
	maxCustomResourceFiles = 1000
	maxCustomResourceBytes = 16 * 1024 * 1024
)

type platformResources struct {
	files map[string][]byte
}

func loadPlatformResources(customRoot string) (platformResources, error) {
	result := platformResources{files: make(map[string][]byte)}
	if err := fs.WalkDir(embeddedResources, "resources", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(name, "resources/")
		data, err := fs.ReadFile(embeddedResources, name)
		if err != nil {
			return err
		}
		result.files[rel] = data
		return nil
	}); err != nil {
		return platformResources{}, err
	}
	if customRoot == "" {
		return result, nil
	}
	count, total := 0, int64(0)
	for _, subdir := range []string{"public", "nginx"} {
		base := filepath.Join(customRoot, subdir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return platformResources{}, fmt.Errorf("inspect custom resources: %w", err)
		}
		err := filepath.WalkDir(base, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("custom resource %s is a symbolic link", name)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("custom resource %s is not a regular file", name)
			}
			count++
			if count > maxCustomResourceFiles {
				return fmt.Errorf("too many custom resource files")
			}
			total += info.Size()
			if total > maxCustomResourceBytes {
				return fmt.Errorf("custom resources exceed %d bytes", maxCustomResourceBytes)
			}
			rel, err := filepath.Rel(customRoot, name)
			if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("custom resource escapes root")
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "public/") {
				// Nginx templates and include fragments are read by the publisher
				// when it renders a candidate. They still pass the same file and
				// size checks above, but do not belong in the standalone resource
				// snapshot.
				return nil
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			rel = strings.TrimPrefix(rel, "public/")
			result.files[rel] = data
			return nil
		})
		if err != nil {
			return platformResources{}, err
		}
	}
	return result, nil
}
