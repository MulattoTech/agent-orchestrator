// Command transferbench validates and compares thread-transfer benchmark runs
// and prepares baseline trees. It treats every artifact as data.
//
//	transferbench compare --baseline B.json --candidate C.json --ceilings ceilings.json \
//	    [--expect-baseline-ref SHA] [--expect-candidate-ref SHA] [--allow-client-change] \
//	    [--out-json report.json] [--out-md report.md]
//	transferbench validate --run R.json
//	transferbench overlay --candidate-root DIR --baseline-root DIR --manifest OUT.json
//	transferbench verify-export --export-root DIR --ls-tree FILE --manifest OVERLAY.json
//
// Exit codes: 0 pass, 1 budget or semantic failure, 2 invalid input,
// incomparable runs, or usage error.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/transferbench"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: transferbench compare|validate|overlay|verify-export [flags]")
		return transferbench.ExitIncomparable
	}
	switch args[0] {
	case "compare":
		return compare(args[1:], stdout, stderr)
	case "validate":
		return validate(args[1:], stdout, stderr)
	case "overlay":
		return overlay(args[1:], stdout, stderr)
	case "verify-export":
		return verifyExport(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	return transferbench.ExitIncomparable
}

func compare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baselinePath := fs.String("baseline", "", "baseline run JSON")
	candidatePath := fs.String("candidate", "", "candidate run JSON")
	ceilingsPath := fs.String("ceilings", "", "reviewed ceilings JSON")
	expectBaseline := fs.String("expect-baseline-ref", "", "commit the baseline must have measured")
	expectCandidate := fs.String("expect-candidate-ref", "", "commit the candidate must have measured")
	allowClientChange := fs.Bool("allow-client-change", false, "compare even when renderer source differs from the simulator's")
	outJSON := fs.String("out-json", "", "write the report JSON here")
	outMD := fs.String("out-md", "", "write the Markdown report here")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *baselinePath == "" || *candidatePath == "" || *ceilingsPath == "" {
		fmt.Fprintln(stderr, "compare needs --baseline, --candidate and --ceilings")
		return transferbench.ExitIncomparable
	}
	ceilingsBytes, err := transferbench.ReadArtifact(*ceilingsPath)
	if err != nil {
		return rejected(stderr, "ceilings", err)
	}
	ceilings, err := transferbench.DecodeCeilings(ceilingsBytes)
	if err != nil {
		return rejected(stderr, "ceilings", err)
	}
	baseline, err := loadRun(*baselinePath)
	if err != nil {
		return rejected(stderr, "baseline run", err)
	}
	candidate, err := loadRun(*candidatePath)
	if err != nil {
		return rejected(stderr, "candidate run", err)
	}
	report := transferbench.Compare(baseline, candidate, ceilings, transferbench.Options{
		CeilingsSha256:     transferbench.SHA256Hex(ceilingsBytes),
		ExpectBaselineRef:  *expectBaseline,
		ExpectCandidateRef: *expectCandidate,
		AllowClientChange:  *allowClientChange,
	})
	markdown := transferbench.RenderMarkdown(report)
	if *outJSON != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(*outJSON, append(data, '\n'), 0o644) != nil { //nolint:gosec // report output.
			fmt.Fprintln(stderr, "could not write the JSON report")
			return transferbench.ExitIncomparable
		}
	}
	if *outMD != "" {
		if err := os.WriteFile(*outMD, []byte(markdown), 0o644); err != nil { //nolint:gosec // report output.
			fmt.Fprintln(stderr, "could not write the Markdown report")
			return transferbench.ExitIncomparable
		}
	}
	fmt.Fprint(stdout, markdown)
	return report.ExitCode
}

func loadRun(path string) (transferbench.Run, error) {
	data, err := transferbench.ReadArtifact(path)
	if err != nil {
		return transferbench.Run{}, err
	}
	return transferbench.DecodeRun(data)
}

func rejected(stderr io.Writer, what string, err error) int {
	fmt.Fprintf(stderr, "rejected %s: %v\n", what, err)
	return transferbench.ExitIncomparable
}

func validate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runPath := fs.String("run", "", "run JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *runPath == "" {
		fmt.Fprintln(stderr, "validate needs --run")
		return transferbench.ExitIncomparable
	}
	if _, err := loadRun(*runPath); err != nil {
		return rejected(stderr, "run", err)
	}
	fmt.Fprintln(stdout, "valid")
	return transferbench.ExitPass
}

func overlay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("overlay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	candidateRoot := fs.String("candidate-root", "", "repository root holding the harness")
	baselineRoot := fs.String("baseline-root", "", "exported baseline tree")
	manifest := fs.String("manifest", "", "write the overlay manifest here")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *candidateRoot == "" || *baselineRoot == "" || *manifest == "" {
		fmt.Fprintln(stderr, "overlay needs --candidate-root, --baseline-root and --manifest")
		return transferbench.ExitIncomparable
	}
	entries, err := transferbench.Overlay(*candidateRoot, *baselineRoot)
	if err != nil {
		fmt.Fprintf(stderr, "overlay refused: %v\n", err)
		return transferbench.ExitIncomparable
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil || os.WriteFile(*manifest, append(data, '\n'), 0o644) != nil { //nolint:gosec // manifest output.
		fmt.Fprintln(stderr, "could not write the overlay manifest")
		return transferbench.ExitIncomparable
	}
	fmt.Fprintf(stdout, "overlaid %d harness files\n", len(entries))
	return transferbench.ExitPass
}

func verifyExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	exportRoot := fs.String("export-root", "", "exported baseline tree")
	lsTreePath := fs.String("ls-tree", "", "output of git ls-tree -r -z --full-tree <commit> -- <paths>")
	manifest := fs.String("manifest", "", "overlay manifest")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *exportRoot == "" || *lsTreePath == "" || *manifest == "" {
		fmt.Fprintln(stderr, "verify-export needs --export-root, --ls-tree and --manifest")
		return transferbench.ExitIncomparable
	}
	lsTree, err := os.ReadFile(*lsTreePath)
	if err != nil {
		return rejected(stderr, "ls-tree output", err)
	}
	var entries []transferbench.OverlayEntry
	manifestBytes, err := os.ReadFile(*manifest)
	if err == nil {
		err = json.Unmarshal(manifestBytes, &entries)
	}
	if err != nil {
		return rejected(stderr, "overlay manifest", errors.New("unreadable"))
	}
	if err := transferbench.VerifyExport(*exportRoot, lsTree, entries); err != nil {
		fmt.Fprintf(stderr, "baseline tree verification failed: %v\n", err)
		return transferbench.ExitIncomparable
	}
	fmt.Fprintln(stdout, "baseline tree matches the commit plus the overlay")
	return transferbench.ExitPass
}
