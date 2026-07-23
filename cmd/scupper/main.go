// Command scupper enforces a REACHABILITY floor over a Go coverage profile: was
// every line executed at least once by some test, or explicitly dismissed with
// a reasoned //scupper:ignore directive? It filters the profile against those
// directives, then reports reachability and (optionally) enforces a threshold.
//
// Reachability is a floor, not a quality signal: "executed at least once" is
// strictly weaker than "tested". A line reached only incidentally (e.g. walked
// past by an e2e flow) counts as reached. The value is catching code that has
// NEVER run under any test — dead branches, an unreachable-in-practice path, a
// stray 1/0 — which is unobserved until a user hits it in production. Test
// quality is a separate axis built on top of this floor.
//
// Feed it a GENEROUS profile — unit + cross-package (-coverpkg) + e2e + tagged,
// merged with `go tool covdata` — so "executed by ANY test" is measured fully.
//
// Typical use in CI:
//
//	go test ./... -coverprofile=cover.out -covermode=set
//	scupper -i cover.out -o cover.filtered.out -threshold 100 --require-reason
//
// Exit codes:
//
//	0  reachability met the threshold (or no threshold set)
//	1  reachability below threshold (some line never executed and not dismissed)
//	2  usage or processing error
package main

import (
	"fmt"
	"os"

	"github.com/sourcehaven-bv/scupper"
)

func main() {
	os.Exit(run())
}

type config struct {
	in            string
	out           string
	threshold     float64
	directive     string
	dir           string
	quiet         bool
	requireReason bool
}

func run() int {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
		usage(os.Stderr)
		return 2
	}

	var src *os.File
	if cfg.in == "" || cfg.in == "-" {
		src = os.Stdin
	} else {
		src, err = os.Open(cfg.in)
		if err != nil {
			fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
			return 2
		}
		defer src.Close()
	}

	prof, err := scupper.ParseProfile(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
		return 2
	}

	dir := cfg.dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	res := scupper.NewResolver(dir)
	d := scupper.DefaultDirectives(cfg.directive)
	d.RequireReason = cfg.requireReason

	result, err := scupper.Filter(prof, res, d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
		return 2
	}

	if cfg.out != "" {
		f, err := os.Create(cfg.out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
			return 2
		}
		if err := result.Kept.Write(f); err != nil {
			f.Close()
			fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
			return 2
		}
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "scupper: "+err.Error())
			return 2
		}
	}

	stats := scupper.Compute(result.Kept)
	unreached := stats.TotalStmts - stats.CoveredStmts
	if !cfg.quiet {
		// Lead with WHAT is unreached — for a reachability floor, the list of
		// never-executed lines is the actionable artifact; the percentage is
		// only "how far". Each line here has NEVER run in any test.
		nBlocks := reportUnreached(os.Stdout, result.Kept)
		if nBlocks > 0 {
			fmt.Printf("\n%d line-block(s) never executed by any test (%d statements).\n", nBlocks, unreached)
			fmt.Println("Each needs a test that reaches it, or a reasoned dismissal.")
		}
		fmt.Printf("reachability: %.1f%% of statements executed (%d/%d)\n",
			stats.Percent(), stats.CoveredStmts, stats.TotalStmts)
		if result.RemovedStmts > 0 {
			fmt.Printf("dismissed: %d statement(s) excluded via %s directives", result.RemovedStmts, d.Line)
			if len(result.IgnoredFiles) > 0 {
				fmt.Printf(" (%d whole file(s))", len(result.IgnoredFiles))
			}
			fmt.Println()
		}
		fmt.Println("note: reachability = executed at least once, NOT tested. Test quality is a separate axis.")
	}

	if cfg.threshold > 0 && stats.Percent()+1e-9 < cfg.threshold {
		fmt.Fprintf(os.Stderr, "scupper: reachability %.1f%% is below threshold %.1f%% (%d line-block(s) never executed)\n",
			stats.Percent(), cfg.threshold, unreached)
		return 1
	}
	return 0
}

// reportUnreached prints each block that has zero hits — code never executed by
// any test in the merged profile — and returns how many it printed. A failing
// run points straight at what needs a test or a reasoned dismissal.
func reportUnreached(w *os.File, p *scupper.Profile) int {
	n := 0
	for _, b := range p.Blocks {
		if b.Count == 0 {
			fmt.Fprintf(w, "  unreached: %s:%d-%d\n", b.File, b.StartLine, b.EndLine)
			n++
		}
	}
	return n
}

func parseArgs(args []string) (config, error) {
	cfg := config{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "-i", "--in", "-input":
			cfg.in, err = next()
		case "-o", "--out", "-output":
			cfg.out, err = next()
		case "-t", "-threshold", "--threshold":
			var v string
			v, err = next()
			if err == nil {
				_, err = fmt.Sscanf(v, "%f", &cfg.threshold)
			}
		case "-d", "-directive", "--directive":
			cfg.directive, err = next()
		case "-dir", "--dir":
			cfg.dir, err = next()
		case "-q", "-quiet", "--quiet":
			cfg.quiet = true
		case "-require-reason", "--require-reason":
			cfg.requireReason = true
		case "-h", "-help", "--help":
			usage(os.Stdout)
			os.Exit(0)
		default:
			return cfg, fmt.Errorf("unknown argument %q", a)
		}
		if err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func usage(w *os.File) {
	fmt.Fprint(w, `scupper — enforce a REACHABILITY floor (every line executed once, or dismissed)

Reachability = executed at least once by some test, NOT tested. It is a floor;
test quality is a separate axis. Feed a generous profile (unit + -coverpkg + e2e
+ tagged, merged via 'go tool covdata') so "executed by ANY test" is measured.

usage:
  scupper [-i profile] [-o filtered] [-threshold N] [-directive base] [-dir path] [-quiet]

flags:
  -i, --in FILE          input coverage profile (default: stdin; "-" also means stdin)
  -o, --out FILE         write the filtered profile here (optional)
  -t, --threshold N      fail (exit 1) if reachability is below N percent
  -d, --directive S      directive base keyword (default: "scupper:ignore";
                         use "coverage-ignore" to read go-test-coverage comments)
      --dir PATH         directory to resolve import paths from (default: cwd)
      --require-reason   a directive without a trailing ": <reason>" is an error
  -q, --quiet            suppress the reachability report on stdout

directives (placed in source comments; <base> defaults to "scupper:ignore"):
  //<base>         ignore the line the comment is on
  //<base>-start   begin an ignored block   (comment may sit anywhere in the block)
  //<base>-end     end an ignored block
  //<base>-file    ignore the whole file

with -d coverage-ignore and --require-reason, this reads rela-style comments:
  // coverage-ignore: <reason>            single line
  // coverage-ignore-start: <reason>      ... // coverage-ignore-end
  // coverage-ignore-file: <reason>       whole file
`)
}
