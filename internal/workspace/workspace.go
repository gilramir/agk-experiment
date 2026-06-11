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

// Resolve turns a caller-supplied path into an absolute path guaranteed to live
// inside the workspace. The input may be:
//   - workspace-relative ("src/Foo.java") — the normal, documented form;
//   - absolute and already inside the workspace ("/checkout/src/Foo.java") —
//     accepted as-is, so an absolute path from the mapper or the model works
//     instead of being re-rooted into a doubled, nonexistent path;
//   - absolute but outside the workspace ("/etc/passwd") — reinterpreted
//     relative to the root so it cannot escape the jail.
//
// Paths that escape the root via "..", or via a symlink, are rejected. Resolve
// is idempotent: feeding it a path it previously returned yields the same file.
func (w *Workspace) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}

	// Build the in-jail interpretations, most-specific first, and prefer
	// whichever actually exists. This is what makes Resolve idempotent: an
	// absolute path already inside the root is kept verbatim rather than being
	// re-rooted onto the root a second time (which produced a doubled,
	// nonexistent path and sent the agent into a retry loop).
	clean := filepath.Clean(p)
	var candidates []string
	if filepath.IsAbs(p) {
		// Recover from a doubled root prefix ("/root/root/x" -> "/root/x"),
		// which is what happens when the model prepends the workspace root to a
		// path that was already absolute.
		if deduped := w.dedupRoot(clean); deduped != clean && within(w.root, deduped) {
			candidates = append(candidates, deduped)
		}
		// An absolute path already inside the jail is kept verbatim.
		if within(w.root, clean) {
			candidates = append(candidates, clean)
		}
	}
	rel := strings.TrimPrefix(clean, string(filepath.Separator))
	candidates = append(candidates, filepath.Clean(filepath.Join(w.root, rel)))

	abs := candidates[0]
	for _, c := range candidates {
		if _, err := os.Lstat(c); err == nil {
			abs = c
			break
		}
	}

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

// dedupRoot collapses one or more accidentally doubled root prefixes, e.g.
// "/root/root/x" -> "/root/x". Returns p unchanged when there is no doubling.
// Used as a recovery candidate only; the result is still subject to the jail
// check in Resolve, so this can never widen access.
func (w *Workspace) dedupRoot(p string) string {
	sep := string(filepath.Separator)
	rootRel := strings.TrimPrefix(w.root, sep) // root without its leading separator
	doubled := w.root + sep + rootRel + sep    // e.g. "/a/b/c/" + "a/b/c/"
	for strings.HasPrefix(p, doubled) {
		p = w.root + sep + strings.TrimPrefix(p, doubled)
	}
	return p
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
