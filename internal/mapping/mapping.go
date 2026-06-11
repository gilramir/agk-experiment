// Package mapping translates a failed Jenkins test into the path of its source
// file within the local workspace.
//
// This is intentionally a STUB. The real translation is project-specific (it
// depends on language, repository layout, and the package -> directory
// convention of the codebase under test), so the user will supply the real
// implementation here.
package mapping

import (
	"github.com/gilbertr/testdiag/internal/jenkins"
)

// Result is the outcome of mapping a test to its source location.
type Result struct {
	// SourceFile is the workspace-relative path to the test's source file
	// (e.g. "src/main/java/com/acme/Foo.java" or "pkg/foo/foo_test.go").
	// May be empty if the mapping is unknown — diagnosis still proceeds and the
	// LLM can locate the file itself with the directory/grep tools.
	SourceFile string

	// Notes is optional human-readable context about how the path was derived;
	// it is included in the prompt to help the LLM.
	Notes string
}

// MapTestToSource translates a failed test to its source file within the
// workspace rooted at workspaceRoot.
//
// TODO(user): implement the real, project-specific mapping. A typical Go/Java
// implementation would derive the path from test.ClassName (e.g. by turning a
// fully-qualified class name into a directory path and locating the file on
// disk under workspaceRoot). Return an empty SourceFile when the path cannot be
// determined; callers treat that as "unknown, let the agent search".
func MapTestToSource(workspaceRoot string, test jenkins.FailedTest) (Result, error) {
	// --- placeholder -----------------------------------------------------
	// The block below is a non-authoritative best-effort guess so the rest of
	// the pipeline has something to show. Replace it with real logic.
	_ = workspaceRoot
	return Result{
		SourceFile: "",
		Notes: "source-file mapping not yet implemented (mapping.MapTestToSource is a stub); " +
			"the failing test is " + test.FullName(),
	}, nil
	// ---------------------------------------------------------------------
}
