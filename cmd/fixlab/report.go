package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/luizeduardocarvalho/fixlab/internal/report"
)

// reportCmd writes the history as one self-contained HTML page.
func reportCmd(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var c common
	c.register(fs)
	dir := fs.String("history", "", "history repo checkout (default: env "+historyEnv+")")
	out := fs.String("o", "", "output file (default: <history>/report.html)")
	_ = fs.Parse(args)

	cfg, err := c.config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	root := resolveHistory(*dir)
	if root == "" {
		fmt.Fprintln(os.Stderr, "no history dir: pass -history or set "+historyEnv)
		return 2
	}
	path := firstNonEmpty(*out, filepath.Join(root, "report.html"))
	d, err := buildData(cfg, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := report.Write(path, d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, w := range d.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	abs, _ := filepath.Abs(path)
	fmt.Printf("wrote %s (%d tasks, %d attempts)\n", abs, len(d.Tasks), len(d.Attempts))
	return 0
}
