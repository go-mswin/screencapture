// Copyright (c) the go-mswin/screencapture authors.
// SPDX-License-Identifier: BSD-3-Clause

package screencapture

// Where a capture may be written, and — the part that matters — where it may
// not.
//
// This file is deliberately UNTAGGED. The live suite that takes captures is
// behind `windows && integration` and runs only on a real desktop, which no CI
// lane has; a guard that only compiles there is a guard nobody runs. The rule
// it enforces is plain filesystem reasoning with nothing Windows about it, so
// it is checked on every platform, on every lane, on every push.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDir is where a live run may put a capture, and the whole of the
// point is where it may NOT.
//
// A screen capture is a picture of whatever the machine happened to be showing
// — a password manager, somebody's mail, an unreleased build. Writing one into
// the working tree puts it one `git add -A` away from being published forever,
// and a .gitignore entry does not prevent that: it is one `git add -f`, one
// tool that ignores it, one person who copies the file elsewhere in the tree.
// So the directory is OUTSIDE every repository, and that is checked rather
// than assumed.
//
// This test suite used to default to testdata/artifacts, INSIDE this
// repository. The committed proof set that lives there was put there
// deliberately, by hand, from a disposable VM; nothing a test RUNS may land
// there again.
func captureDir(t testing.TB) string {
	t.Helper()
	dir, err := chooseCaptureDir(os.Getenv(captureDirEnv))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("capture directory %q: %v", dir, err)
	}
	return dir
}

// captureDirEnv overrides where captures go. It is still checked.
const captureDirEnv = "SCREENCAPTURE_ARTIFACTS"

// chooseCaptureDir is the decision, separated from the test plumbing so the
// REFUSAL can be exercised. A guard whose failing branch never runs is not
// known to work — and this one's failing branch is the entire reason it
// exists.
//
// want is the caller's choice, or "" to use the default.
func chooseCaptureDir(want string) (string, error) {
	// chosen names the directory the way the person reading a failure would:
	// by the variable when they set one, by what it is otherwise.
	dir, chosen := want, captureDirEnv
	if dir == "" {
		chosen = "the default capture directory"
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("no user configuration directory to keep captures in: %w", err)
		}
		dir = filepath.Join(base, "go-mswin-screencapture", "captures")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%s (%q): %w", chosen, dir, err)
	}
	// The refusal is the point. A directory a person chose is still checked,
	// because the mistake this prevents is exactly the one a person makes.
	if root := repoRootOf(abs); root != "" {
		return "", fmt.Errorf("%s (%q) is inside the git work tree at %s; "+
			"a screen capture must never be written where it can be committed", chosen, abs, root)
	}
	return abs, nil
}

// repoRootOf returns the work tree dir is inside, or "" if it is in none. It
// walks all the way to the filesystem root: a capture directory three levels
// below a checkout is still in the checkout.
func repoRootOf(dir string) string {
	for d := dir; ; {
		// A .git that is a FILE is a worktree or a submodule, and commits just
		// as well as a directory does.
		if fi, err := os.Stat(filepath.Join(d, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// The guard has to guard. A capture directory inside a work tree must be
// REFUSED, and this is the only way to find out that it is without writing a
// capture somewhere to see.
func TestCaptureDirRefusesTheWorkTree(t *testing.T) {
	// This test file is in one, so its own directory is the case that matters.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if root := repoRootOf(wd); root == "" {
		t.Fatalf("repoRootOf(%q) found no work tree, and this file is in one", wd)
	}
	// Deep inside it, too — the walk must not stop at the first parent.
	if root := repoRootOf(filepath.Join(wd, "testdata", "artifacts")); root == "" {
		t.Error("repoRootOf did not walk up out of testdata/artifacts")
	}
	// And a directory in no work tree must be accepted. The filesystem root
	// is the one place guaranteed not to be a checkout.
	if root := repoRootOf(filepath.Dir(filepath.VolumeName(wd) + string(filepath.Separator))); root != "" {
		t.Errorf("repoRootOf reported the filesystem root as the work tree %q", root)
	}
}

// And the refusal itself, which is the branch that matters.
func TestChooseCaptureDirRefusesAnythingCommittable(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"this repository", wd},
		{"the directory the proof set lives in", filepath.Join(wd, "testdata", "artifacts")},
		{"a path that does not exist yet, inside the tree", filepath.Join(wd, "no", "such", "place")},
		{"a relative path inside the tree", "testdata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseCaptureDir(tc.want)
			if err == nil {
				t.Fatalf("chooseCaptureDir(%q) = %q, want a refusal", tc.want, got)
			}
			if !strings.Contains(err.Error(), "never be written where it can be committed") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
	// The default must be usable, or every live run fails on a rule that was
	// meant to redirect it rather than stop it.
	got, err := chooseCaptureDir("")
	if err != nil {
		t.Fatalf("the default capture directory was refused: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("default capture directory %q is not absolute", got)
	}
}
