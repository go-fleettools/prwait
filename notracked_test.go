package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
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
func TestNoExecutableIsCommitted(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git holds packed objects, which are not what a tag
			// publishes as source.
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		head := make([]byte, 4)
		n, _ := f.Read(head)
		if kind := executableKind(head[:n]); kind != "" {
			t.Errorf("%s is a %s executable — a tag would publish it", path, kind)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
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
