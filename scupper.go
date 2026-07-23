// Package scupper filters a Go coverage profile, removing blocks that the
// source has explicitly marked as excluded via comment directives. This lets a
// project set a 100% line-coverage target where "100%" means "all code that
// should be covered is covered" — build wiring, impossible error branches, and
// generated code are excluded visibly in the source itself.
//
// A scupper is a deck drain that lets water run off deliberately; here it lets
// explicitly-marked lines drain out of the coverage count.
//
// Four directive styles are supported:
//
//	//scupper:ignore            — ignore the line it appears on (trailing or own-line)
//	//scupper:ignore-start      — begin an ignored block
//	//scupper:ignore-end        — end an ignored block
//	//scupper:ignore-file       — ignore the entire file
//
// The default directive keyword is "scupper:ignore"; it is configurable so a
// project can adopt its own convention.
package scupper

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Directives holds the configured comment keywords. Zero value is not usable;
// call DefaultDirectives.
type Directives struct {
	Line  string // e.g. "scupper:ignore"
	Start string // e.g. "scupper:ignore-start"
	End   string // e.g. "scupper:ignore-end"
	File  string // e.g. "scupper:ignore-file"

	// RequireReason makes a directive without a trailing explanation a hard
	// error. This enforces the "every dismissal is explicit and reviewable"
	// rule — a bare `//coverage-ignore` is rejected; `//coverage-ignore: why`
	// is accepted. The -end directive is exempt (it only closes a block whose
	// -start already carries the reason).
	RequireReason bool
}

// DefaultDirectives returns the standard directive keywords built from base,
// e.g. base "scupper:ignore" yields "scupper:ignore-start" etc. Reasons are not
// required; set RequireReason on the result to enforce them.
func DefaultDirectives(base string) Directives {
	if base == "" {
		base = "scupper:ignore"
	}
	return Directives{
		Line:  base,
		Start: base + "-start",
		End:   base + "-end",
		File:  base + "-file",
	}
}

// FileIgnore describes the ignored line ranges within a single source file.
type FileIgnore struct {
	WholeFile bool
	// Ranges are inclusive [start,end] 1-based line numbers.
	Ranges []Range
}

// Range is an inclusive 1-based line range.
type Range struct{ Start, End int }

// Covers reports whether line n falls within any ignored range (or the whole
// file is ignored).
func (fi FileIgnore) Covers(n int) bool {
	if fi.WholeFile {
		return true
	}
	for _, r := range fi.Ranges {
		if n >= r.Start && n <= r.End {
			return true
		}
	}
	return false
}

// OverlapsRange reports whether the inclusive line span [lo,hi] intersects any
// ignored range (or the whole file is ignored). Used to match a profile block,
// which may span several lines, against a directive that sits on any one of
// them.
func (fi FileIgnore) OverlapsRange(lo, hi int) bool {
	if fi.WholeFile {
		return true
	}
	for _, r := range fi.Ranges {
		if lo <= r.End && r.Start <= hi {
			return true
		}
	}
	return false
}

// commentRe finds a `//` line comment on a source line and captures everything
// after the slashes. A directive must be the FIRST token of that comment — so
// `//scupper:ignore` and `// scupper:ignore` fire, but prose that merely
// mentions the keyword (a doc comment, a README example pasted into a comment,
// a string that happens to contain it) does not. This keeps the scanner from
// false-triggering on its own documentation.
var commentRe = regexp.MustCompile(`//+(.*)$`)

// directiveKind classifies a comment body against the configured directives,
// requiring the keyword to be the leading token. It returns the kind ("file",
// "start", "end", "line") and the trailing reason text (everything after the
// keyword and an optional ":" separator, trimmed). kind is "" if the comment is
// not a directive.
//
// The keyword may be followed by a ":" — so both "scupper:ignore reason" and
// the go-test-coverage style "coverage-ignore: reason" are accepted. The order
// of tests matters: -file/-start/-end are longer than the bare Line keyword, so
// they must win over it.
func directiveKind(commentBody string, d Directives) (kind, reason string) {
	body := strings.TrimLeft(commentBody, " \t")
	// First whitespace-delimited token, minus a trailing ":" separator.
	tok := body
	rest := ""
	if sp := strings.IndexAny(tok, " \t"); sp >= 0 {
		tok, rest = tok[:sp], strings.TrimSpace(tok[sp+1:])
	}
	// A ":" may hang off the keyword ("coverage-ignore:") or sit alone as the
	// separator ("coverage-ignore :"). Normalize both.
	tok = strings.TrimSuffix(tok, ":")
	rest = strings.TrimPrefix(rest, ":")
	reason = strings.TrimSpace(rest)
	switch tok {
	case d.File:
		return "file", reason
	case d.Start:
		return "start", reason
	case d.End:
		return "end", reason
	case d.Line:
		return "line", reason
	}
	return "", ""
}

// keywordFor returns the configured keyword string for a directive kind, used
// in error messages.
func keywordFor(kind string, d Directives) string {
	switch kind {
	case "file":
		return d.File
	case "start":
		return d.Start
	case "end":
		return d.End
	default:
		return d.Line
	}
}

// ScanFile reads path and returns the ignored ranges implied by its directive
// comments.
//
// Block directives must be balanced and correctly ordered. Every misuse is a
// hard error rather than a silent swallow — because a tool whose value is
// "exclusions are visible and reviewable" must not quietly mis-handle a
// mistyped directive:
//
//   - an ignore-end with no open block (a stray or swapped end);
//   - a second ignore-start while a block is already open (blocks do not nest);
//   - an ignore-start with no matching ignore-end (unterminated at EOF).
func ScanFile(path string, d Directives) (FileIgnore, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileIgnore{}, err
	}
	defer f.Close()

	var fi FileIgnore
	var (
		inBlock    bool
		blockStart int
		lineNo     int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lineNo++
		text := sc.Text()
		m := commentRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		kind, reason := directiveKind(m[1], d)
		// Enforce an explanation on every directive that opens an exclusion.
		// -end is exempt: it merely closes a block whose -start carries the why.
		if d.RequireReason && reason == "" && kind != "" && kind != "end" {
			return fi, fmt.Errorf("%s:%d: %q requires an explanation (write %q: <reason>)",
				path, lineNo, keywordFor(kind, d), keywordFor(kind, d))
		}
		switch kind {
		case "file":
			fi.WholeFile = true
		case "start":
			if inBlock {
				return fi, fmt.Errorf("%s:%d: %q with a block already open at line %d (blocks do not nest; close it with %q first)",
					path, lineNo, d.Start, blockStart, d.End)
			}
			inBlock = true
			blockStart = lineNo
		case "end":
			if !inBlock {
				return fi, fmt.Errorf("%s:%d: %q with no open block (missing or swapped %q)",
					path, lineNo, d.End, d.Start)
			}
			fi.Ranges = append(fi.Ranges, Range{blockStart, lineNo})
			inBlock = false
		case "line":
			fi.Ranges = append(fi.Ranges, Range{lineNo, lineNo})
		}
	}
	if err := sc.Err(); err != nil {
		return FileIgnore{}, err
	}
	if inBlock {
		return fi, fmt.Errorf("%s:%d: unterminated %q (missing %q)", path, blockStart, d.Start, d.End)
	}
	return fi, nil
}
