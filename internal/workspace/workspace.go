// Package workspace provides a jailed view of the local checkout. All
// file-reading tools resolve paths through a Workspace so that the LLM cannot
// read files outside the build's source tree.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is a directory the file tools are confined to.
type Workspace struct {
	root string // absolute, symlink-resolved
}

// New creates a Workspace rooted at the given directory. The path must exist
// and be a directory.
func New(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks on the root so prefix checks in Resolve are sound.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", root)
	}
	return &Workspace{root: abs}, nil
}

// Root returns the absolute workspace root.
func (w *Workspace) Root() string { return w.root }

// Resolve turns a caller-supplied (possibly relative) path into an absolute
// path guaranteed to live inside the workspace. Paths that escape the root via
// "..", absolute paths, or symlinks are rejected.
func (w *Workspace) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}

	// Treat the input as relative to the workspace root. An absolute input is
	// reinterpreted relative to the root rather than the real filesystem root.
	rel := p
	if filepath.IsAbs(p) {
		rel = strings.TrimPrefix(p, string(filepath.Separator))
	}
	abs := filepath.Join(w.root, rel)
	abs = filepath.Clean(abs)

	// Resolve symlinks on the deepest existing ancestor, then re-check the
	// prefix so a symlink pointing outside the jail cannot be followed.
	checked := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		checked = resolved
	} else if resolved, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		checked = filepath.Join(resolved, filepath.Base(abs))
	}

	if !within(w.root, checked) {
		return "", fmt.Errorf("path %q escapes the workspace root", p)
	}
	return checked, nil
}

// Rel returns abs expressed relative to the workspace root (for display).
func (w *Workspace) Rel(abs string) string {
	if r, err := filepath.Rel(w.root, abs); err == nil {
		return r
	}
	return abs
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
