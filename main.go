// Atlas: dependency graph analysis and multi-agent risk assessment.
// Supports npm (package-lock.json) and Python/poetry (poetry.lock) projects.
//
// Usage:
//
//	go run . -repo owner/repo -ref main -dir packages/app
//	go run . -file ./package-lock.json          # one-shot: local npm lockfile
//	go run . -file ./poetry.lock                # one-shot: local poetry lockfile (needs sibling pyproject.toml)
//	go run . -file ./package-lock.json -top 30
//	go run .                                    # launches the interactive agent
package main

import (
	"atlas/agents"
	"atlas/core"
	"atlas/core/python"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
)

func main() {
	core.LoadDotEnv(".env")

	repo := flag.String("repo", "", "GitHub repo as owner/name")
	ref := flag.String("ref", "", "branch, tag or sha (default: default branch)")
	dir := flag.String("dir", "", "subdirectory containing the lockfile (monorepos)")
	file := flag.String("file", "", "local lockfile (package-lock.json or poetry.lock) instead of fetching")
	top := flag.Int("top", 25, "rows to print")
	src := flag.String("src", "", "local source dir to scan for usage (default: shallow-clone -repo, or dir of -file)")
	show := flag.String("show", "", "print sampled usage snippets for this package")
	viz := flag.String("viz", "", "write an interactive HTML graph to this file, e.g. graph.html")
	vizMax := flag.Int("viz-max", 150, "max nodes in the visualization (root + direct deps always included)")
	health := flag.Bool("health", true, "fetch dependency health signals (package registry, GitHub, OSV.dev); -health=false to skip")
	healthTop := flag.Int("health-top", 0, "also check this many additional packages by blast-radius rank")
	healthJSON := flag.String("health-json", "", "write the health report as JSON to this path")
	model := flag.String("model", "gemma4:e4b", "Ollama model to use for the interactive agent (used when no -repo/-file is given)")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile to this file (non-agent mode only)")
	memprofile := flag.String("memprofile", "", "write a heap profile to this file on exit (non-agent mode only)")
	traceOut := flag.String("trace", "", "write an execution trace to this file (non-agent mode only)")
	flag.Parse()

	if *file == "" {
		agents.RunAgentMode(*model, *repo, *ref, *dir)
		return
	}

	// One-shot mode doesn't use Ollama, and a token is optional (public repos
	// work without one). If a token is set and GitHub will be called, check
	// that GitHub accepts it: a rejected token fails every request, public
	// repositories included.
	pre := core.Preflight(core.PreflightOptions{CheckGithub: *repo != "" || *health})
	for _, w := range pre.Warnings {
		fmt.Fprintln(os.Stderr, core.FormatWarning(w))
	}
	if len(pre.Blocking) > 0 {
		fmt.Fprintln(os.Stderr, core.FormatErrors(pre.Blocking, core.PreflightHeader))
		os.Exit(1)
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cpuprofile:", err)
			os.Exit(1)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintln(os.Stderr, "cpuprofile:", err)
			os.Exit(1)
		}
		defer pprof.StopCPUProfile()
	}
	if *traceOut != "" {
		f, err := os.Create(*traceOut)
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace:", err)
			os.Exit(1)
		}
		defer f.Close()
		if err := trace.Start(f); err != nil {
			fmt.Fprintln(os.Stderr, "trace:", err)
			os.Exit(1)
		}
		defer trace.Stop()
	}
	if *memprofile != "" {
		defer func() {
			f, err := os.Create(*memprofile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "memprofile:", err)
				return
			}
			defer f.Close()
			runtime.GC()
			_ = pprof.WriteHeapProfile(f)
		}()
	}

	data, err := core.ReadLocalFile(*file, "lockfile")
	lockName := filepath.Base(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, core.FormatError(err))
		os.Exit(1)
	}

	var g *core.Graph
	var adapter core.EcosystemAdapter = core.NpmAdapter{}
	switch core.DetectEcosystem(lockName) {
	case core.EcosystemPoetry:
		adapter = python.Adapter{}
		g, err = python.BuildPoetryGraph(data, *repo, *ref, *dir, *file)
		if err != nil {
			fmt.Fprintln(os.Stderr, core.FormatError(err))
			os.Exit(1)
		}
	default:
		var lf core.Lockfile
		if err := json.Unmarshal(data, &lf); err != nil {
			src := "package-lock.json from " + *repo
			if *file != "" {
				src = *file
			}
			fmt.Fprintln(os.Stderr, core.FormatError(core.LockfileParseError(err, src)))
			os.Exit(1)
		}
		if len(lf.Packages) == 0 && len(lf.V1Deps) > 0 {
			pj, err := core.LoadPackageJSON(*repo, *ref, *dir, *file)
			if err != nil {
				fmt.Fprintln(os.Stderr, core.FormatError(core.Annotate(err,
					"This is a version 1 lockfile, which does not list the project's direct dependencies, so Atlas also needs its package.json.")))
				os.Exit(1)
			}
			core.ConvertV1(&lf, pj)
		}
		g, err = core.BuildGraph(&lf)
		if err != nil {
			fmt.Fprintln(os.Stderr, core.FormatError(err))
			os.Exit(1)
		}
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
		rows := core.RunHealth(g, *healthTop, adapter)
		core.PrintHealth(rows, g.Ecosystem)
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
			fmt.Fprintln(os.Stderr, core.FormatError(err))
			os.Exit(1)
		}
		defer cleanup()
		srcDir = filepath.Join(d, *dir)
	} else if srcDir == "" && *file != "" {
		srcDir = filepath.Dir(*file)
	}
	if srcDir != "" {
		core.RunScan(g, srcDir, *show, true, adapter)
	}
}
