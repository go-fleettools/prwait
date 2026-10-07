package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// executableKind names the executable format a file starts with, if any.
//
// It knows nothing about FILE NAMES, and that is the point: the obvious
// guard is a .gitignore line naming the binary, and a rule that names a
// binary dies at the rename, silently.
func executableKind(head []byte) string {
	for _, m := range []struct {
		name  string
		bytes []byte
	}{
		{"ELF", []byte{0x7f, 'E', 'L', 'F'}},
		{"Mach-O 64-bit", []byte{0xcf, 0xfa, 0xed, 0xfe}},
		{"Mach-O 32-bit", []byte{0xce, 0xfa, 0xed, 0xfe}},
		{"Mach-O big-endian", []byte{0xfe, 0xed, 0xfa, 0xcf}},
		{"Mach-O universal", []byte{0xca, 0xfe, 0xba, 0xbe}},
		{"PE/COFF", []byte{'M', 'Z'}},
		{"WebAssembly", []byte{0x00, 'a', 's', 'm'}},
	} {
		if bytes.HasPrefix(head, m.bytes) {
			return m.name
		}
	}
	return ""
}

// NOTHING IN THIS TREE IS AN EXECUTABLE.
//
// go-fleettools/fleettools v0.1.0 shipped 8.4 MB of built binaries, because
// a first tag freezes whatever is in the tree and nobody had looked — and a
// tag cannot be unmade for anyone who already fetched it.
//
// It runs on every lane, so it also catches a binary built on one operating
// system and committed from another.
//
// # IT ASKS GIT, AND THE FIRST VERSION DID NOT
//
// The first version walked the directory, and went red on all three lanes
// at once — on the binary the CI's own build step had just written beside
// the source. That binary is a build artefact, not a commit, and the
// invariant is about what a TAG WOULD PUBLISH. The walk could not tell the
// difference because the question it asked was about the filesystem, and
// the question that matters is about the index.
//
// Locally it had passed, for the worst possible reason: this developer
// happened not to have built in the tree. A guard whose verdict depends on
// where someone last ran go build is not a guard.
func TestNoExecutableIsCommitted(t *testing.T) {
	for _, path := range trackedFiles(t) {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			// Tracked and deleted in the work tree: still not a committed
			// executable, and not this test's business.
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		head := make([]byte, 4)
		n, _ := f.Read(head)
		f.Close()
		if kind := executableKind(head[:n]); kind != "" {
			t.Errorf("%s is a %s executable — a tag would publish it", path, kind)
		}
	}
}

// trackedFiles is everything git would publish from this directory.
//
// A skip here is NAMED. A guard that quietly passes because its instrument
// is missing is the same silence as a guard that passes because the tree is
// clean, and the two must not look alike.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git here, so nothing can say what is tracked")
	}
	out, err := exec.Command(git, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("git ls-files: %v (not a work tree?)", err)
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, filepath.FromSlash(p))
		}
	}
	if len(files) == 0 {
		t.Fatal("git tracks no file here — the guard would pass on an empty set")
	}
	return files
}

// THE GUARD MUST BE ABLE TO FAIL. A check proved only on a clean tree
// proves only that the tree is clean, which is the shape of every guard
// that was quietly doing nothing.
//
// Proving it on a real commit would mean committing a binary, so it is
// proved on the one thing the guard actually reads: the first bytes.
func TestTheExecutableGuardRecognisesOne(t *testing.T) {
	for _, head := range [][]byte{
		{0x7f, 'E', 'L', 'F'},
		{0xcf, 0xfa, 0xed, 0xfe},
		{'M', 'Z', 0x90, 0x00},
		{0x00, 'a', 's', 'm'},
	} {
		if executableKind(head) == "" {
			t.Errorf("%x was not recognised as an executable", head)
		}
	}
	// And what must NOT trip it: ordinary source, and short files, which a
	// prefix test on a truncated read gets wrong in the other direction.
	for _, s := range []string{"package main\n", "# prwait\n", "", "M", "\x7f"} {
		if kind := executableKind([]byte(s)); kind != "" {
			t.Errorf("%q was called a %s", s, kind)
		}
	}
}
