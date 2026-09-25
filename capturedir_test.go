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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-appdirs/outdir"
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
	dir, err := outdir.Ensure(captureSpec(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("captures go to %s", dir)
	return dir
}

// captureDirEnv overrides where captures go. It is still checked.
const captureDirEnv = "SCREENCAPTURE_ARTIFACTS"

// captureSpec is this repository's answer to "where may a capture go", handed
// to the package that owns the question.
//
// ⛔ This used to be sixty lines here, and the same sixty lines lived in
// go-macos/screencapture, go-widgets/window and go-aiquota/tray.
// go-appdirs/outdir is the decision written once -- and adopting it FIXED
// something rather than tidying. The copy walked up from the path AS GIVEN,
// resolving nothing, so a capture directory reached through a symbolic link
// found no work tree and was accepted. outdir resolves first and refuses.
// Same default directory, so nothing moves.
func captureSpec(want string) outdir.Spec {
	return outdir.Spec{
		App:  "go-mswin-screencapture",
		Env:  captureDirEnv,
		Sub:  "captures",
		Want: want,
	}
}

// chooseCaptureDir is the decision, separated from the test plumbing so the
// REFUSAL can be exercised without writing a capture somewhere to find out.
func chooseCaptureDir(want string) (string, error) { return outdir.Choose(captureSpec(want)) }

// repoRootOf is outdir's, kept under this name because the tests read better.
func repoRootOf(dir string) string { return outdir.RepoRootOf(dir) }

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
			// ⛔ The assertion is on what the refusal has to TELL somebody,
			// not on its wording. It used to match a phrase this package
			// wrote itself; the phrase belongs to go-appdirs/outdir now and
			// reads differently, while the behaviour did not change. A test
			// that pins prose fails on a rename and passes on a silent
			// change of meaning.
			root := repoRootOf(wd)
			if !strings.Contains(err.Error(), "work tree") ||
				!strings.Contains(err.Error(), root) {
				t.Errorf("the refusal does not name the work tree it found: %v", err)
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

// ⛔ THE HOLE THE LOCAL COPY HAD. It walked up from the path as given,
// resolving nothing, so a capture directory reached through a symbolic link
// found no .git and was accepted. outdir resolves the path first.
func TestALinkIntoAWorkTreeIsStillTheWorkTree(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(wd, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if root := repoRootOf(link); root == "" {
		t.Error("a link into this work tree was not recognised as being in it")
	}
	if _, err := chooseCaptureDir(link); err == nil {
		t.Error("a capture directory reached through a link into a work tree was accepted")
	}
}
