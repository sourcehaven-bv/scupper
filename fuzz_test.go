package scupper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseProfile asserts the two invariants of the hand-written profile
// parser against arbitrary bytes:
//
//  1. It never panics — arbitrary input yields either a valid *Profile or a
//     non-nil error, never a crash.
//  2. Anything it accepts round-trips: re-serializing the parsed profile and
//     re-parsing yields an identical profile. This catches a parse that
//     silently drops or mangles data it claimed to accept.
//
// Run continuously with: go test -run x -fuzz FuzzParseProfile
func FuzzParseProfile(f *testing.F) {
	seeds := []string{
		"mode: set\n",
		"mode: set\nexample.com/m/f.go:3.24,4.11 1 1\n",
		"mode: set\nexample.com/m/f.go:3.24,4.11 1 1\nexample.com/m/f.go:4.11,6.3 2 0\n",
		"mode: count\na:1.1,1.2 1 5\n",
		"mode: atomic\nweird:name:with:colons.go:1.1,2.2 3 0\n",
		"",
		"garbage",
		"mode: set\n:.,. 0 0\n",
		"mode: set\nx:9999999999999999999999.1,2.2 1 1\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		p, err := ParseProfile(strings.NewReader(in))
		if err != nil {
			return // rejecting input is fine; only crashes/mis-accepts are bugs
		}
		if p == nil {
			t.Fatal("nil profile with nil error")
		}
		// Round-trip: write it back out, parse again, compare.
		var sb strings.Builder
		if err := p.Write(&sb); err != nil {
			t.Fatalf("Write failed on accepted profile: %v", err)
		}
		p2, err := ParseProfile(strings.NewReader(sb.String()))
		if err != nil {
			t.Fatalf("re-parse of serialized profile failed: %v\nserialized:\n%s", err, sb.String())
		}
		if p.Mode != p2.Mode || len(p.Blocks) != len(p2.Blocks) {
			t.Fatalf("round-trip mismatch: mode %q/%q blocks %d/%d",
				p.Mode, p2.Mode, len(p.Blocks), len(p2.Blocks))
		}
		for i := range p.Blocks {
			if p.Blocks[i] != p2.Blocks[i] {
				t.Fatalf("block %d changed across round-trip:\n %+v\n %+v", i, p.Blocks[i], p2.Blocks[i])
			}
		}
	})
}

// FuzzScanFile asserts the scanner never panics on arbitrary file content and
// upholds its balance invariant: the returned ranges are well-formed
// (Start<=End, within the file) and an unterminated block is reported as an
// error rather than silently producing a bogus range.
//
// Run continuously with: go test -run x -fuzz FuzzScanFile
func FuzzScanFile(f *testing.F) {
	seeds := []string{
		"package x\n//scupper:ignore\nfunc f(){}\n",
		"//scupper:ignore-file\npackage x\n",
		"//scupper:ignore-start\ncode\n//scupper:ignore-end\n",
		"//scupper:ignore-start\nno end\n",
		"// scupper:ignore with a note\n",
		"// prose mentioning scupper:ignore not a directive\n",
		"//scupper:ignore-end\n", // end with no start
		"/*scupper:ignore*/\n",
		"",
		"\x00\xff\n//scupper:ignore\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	d := DefaultDirectives("")
	f.Fuzz(func(t *testing.T, content string) {
		// ScanFile reads from disk; write the fuzz input to a temp file.
		path := filepath.Join(t.TempDir(), "src.go")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, err := ScanFile(path, d)
		if err != nil {
			// The only expected errors are balance/ordering violations of block
			// directives. Any other error kind is a bug.
			msg := err.Error()
			if !strings.Contains(msg, "unterminated") &&
				!strings.Contains(msg, "no open block") &&
				!strings.Contains(msg, "already open") {
				t.Fatalf("unexpected error kind: %v", err)
			}
			return
		}
		lineCount := 1 + strings.Count(content, "\n")
		for _, r := range fi.Ranges {
			if r.Start < 1 || r.End < r.Start {
				t.Fatalf("malformed range %+v from content %q", r, content)
			}
			if r.Start > lineCount || r.End > lineCount {
				t.Fatalf("range %+v exceeds %d lines: %q", r, lineCount, content)
			}
		}
	})
}
