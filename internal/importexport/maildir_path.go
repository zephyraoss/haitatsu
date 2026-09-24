package importexport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrMaildirImportDisabled  = errors.New("maildir import is disabled: workers.maildir_import_root is not configured")
	ErrMaildirPathOutsideRoot = errors.New("maildir source.path must be inside workers.maildir_import_root")
)

// ResolveMaildirPath confines a caller-supplied maildir path to root. Relative
// paths are joined onto root; absolute paths must already lie inside it. Symlinks
// are followed so a link inside root cannot point outside of it.
func ResolveMaildirPath(root string, requested string) (string, error) {
	root = strings.TrimSpace(root)
	requested = strings.TrimSpace(requested)
	if root == "" {
		return "", ErrMaildirImportDisabled
	}
	if requested == "" {
		return "", fmt.Errorf("maildir import requires source.path")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("workers.maildir_import_root must be an absolute path")
	}
	root = filepath.Clean(root)
	resolved := filepath.Clean(requested)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, resolved)
	}
	if !withinRoot(root, resolved) {
		return "", ErrMaildirPathOutsideRoot
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workers.maildir_import_root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("maildir source.path does not exist: %w", err)
		}
		return "", fmt.Errorf("resolve maildir source.path: %w", err)
	}
	if !withinRoot(realRoot, realPath) {
		return "", ErrMaildirPathOutsideRoot
	}
	return realPath, nil
}

func withinRoot(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
