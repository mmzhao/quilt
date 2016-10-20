package inspect

import (
	"bufio"
	"fmt"
	"os"
	"text/scanner"

	"github.com/NetSys/quilt/stitch"
)

// Usage prints the usage string for the inspect tool.
func Usage() {
	fmt.Fprintln(
		os.Stderr,
		`quilt inspect is a tool that helps visualize Stitch specifications.
Usage: quilt inspect <path to spec file> <pdf|ascii>
Dependencies
 - easy-graph (install Graph::Easy from cpan)
 - graphviz (install from your favorite package manager)`,
	)
}

// Main is the main function for inspect tool. Helps visualize stitches.
func Main(opts []string) int {
	if arglen := len(opts); arglen < 2 {
		fmt.Println("not enough arguments: ", arglen)
		Usage()
		return 1
	}

	configPath := opts[0]

	f, err := os.Open(configPath)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	defer f.Close()

	sc := scanner.Scanner{
		Position: scanner.Position{
			Filename: configPath,
		},
	}
	compiled, err := stitch.Compile(*sc.Init(bufio.NewReader(f)),
		stitch.DefaultImportGetter)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	spec, err := stitch.New(compiled)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	graph, err := stitch.InitializeGraph(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	switch opts[1] {
	case "pdf":
		fallthrough
	case "ascii":
		viz(configPath, spec, graph, opts[1])
	default:
		Usage()
		return 1
	}

	return 0
}
