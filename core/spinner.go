package core

// A small animated terminal status line: a rotating glyph plus an elapsed
// timer, redrawn in place (like "✶ Fetching lockfile… (2s)") so long-running
// steps show visible, live progress instead of a static line that sits there
// with no feedback until it's done.

import (
	"fmt"
	"time"
)

var spinnerFrames = []string{"✶", "✸", "✹", "✺", "✹", "✷"}

// Spinner represents an animated terminal spinner.
type Spinner struct {
	stop chan struct{}
	done chan struct{}
}

// StartSpinner renders an animated, elapsed-time status line until stopped.
func StartSpinner(label string) *Spinner {
	s := &Spinner{stop: make(chan struct{}), done: make(chan struct{})}
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

// Finish stops the spinner and leaves a permanent ✓/✗ status line behind.
func (s *Spinner) Finish(ok bool, label string) {
	close(s.stop)
	<-s.done
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Printf("%s %s\n", mark, label)
}

// Detail prints a short, dimmed sub-line under the most recent ✓/✗ line.
func Detail(format string, args ...any) {
	fmt.Printf("  \x1b[2m%s\x1b[0m\n", fmt.Sprintf(format, args...))
}

// StopSilent stops the spinner and clears the line with no trace left behind.
func (s *Spinner) StopSilent() {
	close(s.stop)
	<-s.done
}

// WithSpinner runs fn while showing an animated status line for label, then
// leaves a ✓/✗ line behind once it's done.
func WithSpinner(label string, fn func() error) error {
	s := StartSpinner(label)
	err := fn()
	s.Finish(err == nil, label)
	return err
}

// WithSpinnerResult is a generic helper that wraps WithSpinner for functions
// that return a value alongside the error.
func WithSpinnerResult[T any](label string, fn func() (T, error)) (T, error) {
	var val T
	err := WithSpinner(label, func() error {
		var e error
		val, e = fn()
		return e
	})
	return val, err
}
