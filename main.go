// Atlas: dependency graph analysis and multi-agent risk assessment.
//
// Usage:
//	go run . -repo expressjs/express            # fetches package-lock.json from default branch
//	go run . -repo owner/repo -ref main -dir packages/app
//	go run . -file ./package-lock.json          # local file
//	go run . -repo owner/repo -top 30
//	go run .                                    # launches multi-agent interactive REPL
package main

import (
	"atlas/agents"
	"atlas/core"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	core.LoadDotEnv(".env")

	repo := flag.String("repo", "", "GitHub repo as owner/name")
	ref := flag.String("ref", "", "branch, tag or sha (default: default branch)")
	dir := flag.String("dir", "", "subdirectory containing package-lock.json (monorepos)")
	file := flag.String("file", "", "local package-lock.json instead of fetching")
	top := flag.Int("top", 25, "rows to print")
	src := flag.String("src", "", "local source dir to scan for usage (default: shallow-clone -repo, or dir of -file)")
	show := flag.String("show", "", "print sampled usage snippets for this package")
	viz := flag.String("viz", "", "write an interactive HTML graph to this file, e.g. graph.html")
	vizMax := flag.Int("viz-max", 150, "max nodes in the visualization (root + direct deps always included)")
	health := flag.Bool("health", true, "fetch dependency health signals (npm registry, GitHub, OSV.dev); -health=false to skip")
	healthTop := flag.Int("health-top", 0, "also check this many additional packages by blast-radius rank")
	healthJSON := flag.String("health-json", "", "write the health report as JSON to this path")
	model := flag.String("model", "gemma4:e4b", "Ollama model to use for the interactive agent (used when no -repo/-file is given)")
	flag.Parse()

	if *repo == "" && *file == "" {
		agents.RunAgentMode(*model)
		return
	}

	var data []byte
	var err error
	switch {
	case *file != "":
		data, err = os.ReadFile(*file)
	case *repo != "":
		data, err = core.FetchFile(*repo, *ref, *dir, "package-lock.json")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	var lf core.Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		fmt.Fprintln(os.Stderr, "parse error:", err)
		os.Exit(1)
	}
	if len(lf.Packages) == 0 && len(lf.V1Deps) > 0 {
		pj, err := core.LoadPackageJSON(*repo, *ref, *dir, *file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: v1 lockfile needs package.json:", err)
			os.Exit(1)
		}
		core.ConvertV1(&lf, pj)
	}
	g, err := core.BuildGraph(&lf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Print(core.GraphReport(g, *top))

	if *viz != "" {
		if err := core.WriteViz(g, *viz, *vizMax); err != nil {
			fmt.Fprintln(os.Stderr, "viz error:", err)
		} else {
			fmt.Printf("\nWrote interactive graph to %s (open it in a browser)\n", *viz)
		}
	}

	if *health {
		rows := core.RunHealth(g, *healthTop)
		core.PrintHealth(rows)
		if *healthJSON != "" {
			if err := core.WriteHealthJSON(rows, *healthJSON); err != nil {
				fmt.Fprintln(os.Stderr, "health json error:", err)
			} else {
				fmt.Printf("\nWrote health report to %s\n", *healthJSON)
			}
		}
	}

	srcDir := *src
	if srcDir == "" && *repo != "" {
		d, cleanup, err := core.CloneRepo(*repo, *ref)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clone error:", err)
			os.Exit(1)
		}
		defer cleanup()
		srcDir = filepath.Join(d, *dir)
	} else if srcDir == "" && *file != "" {
		srcDir = filepath.Dir(*file)
	}
	if srcDir != "" {
		core.RunScan(g, srcDir, *show, true)
	}
}
