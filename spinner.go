package main

// A small animated terminal status line: a rotating glyph plus an elapsed
// timer, redrawn in place (like "✶ Fetching lockfile… (2s)") so long-running
// steps show visible, live progress instead of a static line that sits there
// with no feedback until it's done.

import (
	"fmt"
	"time"
)

var spinnerFrames = []string{"✶", "✸", "✹", "✺", "✹", "✷"}

type spinner struct {
	stop chan struct{}
	done chan struct{}
}

// startSpinner renders an animated, elapsed-time status line until stopped.
func startSpinner(label string) *spinner {
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		start := time.Now()
		frame := 0
		render := func() {
			elapsed := int(time.Since(start).Seconds())
			fmt.Printf("\r\x1b[2K%s %s… (%ds)", spinnerFrames[frame%len(spinnerFrames)], label, elapsed)
		}
		render()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				fmt.Print("\r\x1b[2K")
				return
			case <-ticker.C:
				frame++
				render()
			}
		}
	}()
	return s
}

// finish stops the spinner and leaves a permanent ✓/✗ status line behind —
// used for steps worth a visible record once they're done (fetched the
// lockfile, cloned the repo, etc.).
func (s *spinner) finish(ok bool, label string) {
	close(s.stop)
	<-s.done
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Printf("%s %s\n", mark, label)
}

// detail prints a short, dimmed sub-line under the most recent ✓/✗ line —
// the gist of what a stage found (counts, a path), not the full table.
func detail(format string, args ...any) {
	fmt.Printf("  \x1b[2m%s\x1b[0m\n", fmt.Sprintf(format, args...))
}

// stopSilent stops the spinner and clears the line with no trace left behind
// — used for the "waiting on the model" spinner, where there's nothing
// worth permanently logging once it's done.
func (s *spinner) stopSilent() {
	close(s.stop)
	<-s.done
}

// withSpinner runs fn while showing an animated status line for label, then
// leaves a ✓/✗ line behind once it's done.
func withSpinner(label string, fn func() error) error {
	s := startSpinner(label)
	err := fn()
	s.finish(err == nil, label)
	return err
}
