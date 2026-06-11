package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestWorkspace creates a workspace root with a known layout:
//
//	<root>/src/Foo.java
//	<root>/pkg/foo/foo.go
//
// It returns the workspace and the canonical (symlink-resolved) root, so tests
// can compare against the same form Resolve produces.
func newTestWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	// New canonicalizes the root via EvalSymlinks; mirror that here so equality
	// checks line up (t.TempDir may sit under a symlinked /tmp on some systems).
	canonRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(canonRoot, "src"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(canonRoot, "pkg", "foo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(canonRoot, "src", "Foo.java"), []byte("class Foo {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(canonRoot, "pkg", "foo", "foo.go"), []byte("package foo\n"), 0o644))

	ws, err := New(root)
	require.NoError(t, err)
	require.Equal(t, canonRoot, ws.Root())
	return ws, canonRoot
}

func TestResolve(t *testing.T) {
	ws, root := newTestWorkspace(t)
	fooJava := filepath.Join(root, "src", "Foo.java")

	tests := []struct {
		name    string
		input   string
		want    string // expected resolved absolute path (empty when wantErr)
		wantErr bool
	}{
		{
			name:  "workspace-relative path",
			input: "src/Foo.java",
			want:  fooJava,
		},
		{
			name:  "dot-prefixed relative path",
			input: "./pkg/foo/foo.go",
			want:  filepath.Join(root, "pkg", "foo", "foo.go"),
		},
		{
			// The bug this guards against: an absolute in-jail path must be kept
			// verbatim, not re-rooted onto the root a second time.
			name:  "absolute path already inside the jail is kept verbatim",
			input: fooJava,
			want:  fooJava,
		},
		{
			// A doubled path (model prepended the root to an already-absolute
			// path) is collapsed back to the real file.
			name:  "doubled root prefix resolves to the real file",
			input: filepath.Join(root, root, "src", "Foo.java"),
			want:  fooJava,
		},
		{
			// An absolute path outside the jail is reinterpreted relative to the
			// root (it never reaches the real /etc/passwd).
			name:  "absolute path outside jail is reinterpreted under root",
			input: "/etc/passwd",
			want:  filepath.Join(root, "etc", "passwd"),
		},
		{
			name:    "parent escape is rejected",
			input:   "../escape",
			wantErr: true,
		},
		{
			name:    "deep parent escape is rejected",
			input:   "src/../../../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "empty path is rejected",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ws.Resolve(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestResolveIdempotent verifies that feeding Resolve a path it already
// produced yields the same file — the property that prevents the agent
// retry-loop on doubled paths.
func TestResolveIdempotent(t *testing.T) {
	ws, _ := newTestWorkspace(t)

	once, err := ws.Resolve("src/Foo.java")
	require.NoError(t, err)

	twice, err := ws.Resolve(once)
	require.NoError(t, err)
	assert.Equal(t, once, twice)

	thrice, err := ws.Resolve(twice)
	require.NoError(t, err)
	assert.Equal(t, once, thrice)
}

// TestResolveSymlinkEscape verifies a symlink pointing outside the workspace
// cannot be followed, even though its own path lies within the root.
func TestResolveSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	ws, root := newTestWorkspace(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("top secret\n"), 0o644))

	link := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, link))

	_, err := ws.Resolve("escape/secret.txt")
	assert.Error(t, err, "symlink pointing outside the jail must be rejected")
}
