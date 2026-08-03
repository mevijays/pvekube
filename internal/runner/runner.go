// Package runner executes external commands (docker, packer, kind,
// clusterctl, kubectl) with output streamed line-by-line to a job step's
// logger and full context-cancellation support so a job Cancel actually
// kills the child process instead of orphaning it.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// LineWriter is the subset of jobs.Ctx we need — kept as an interface so this
// package doesn't import jobs (avoiding an import cycle) and is independently
// testable.
type LineWriter interface {
	Logf(format string, args ...any)
}

// Run executes name(args...) with the given env additions (appended to the
// current process env), streaming combined stdout+stderr line by line to log.
// Returns an error including the last lines of output on non-zero exit, and
// respects ctx cancellation by killing the process group.
func Run(ctx context.Context, log LineWriter, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	cmd.Cancel = func() error {
		return cmd.Process.Kill()
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // combine streams into one ordered log

	log.Logf("$ %s", commandLine(name, args))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	tail := newRingBuffer(40)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		log.Logf("%s", line)
		tail.add(line)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		log.Logf("(log scan error: %v)", err)
	}

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("%s: cancelled", name)
	}
	if waitErr != nil {
		return fmt.Errorf("%s failed: %w\n--- last output ---\n%s", name, waitErr, tail.String())
	}
	return nil
}

// Capture runs a short-lived command (bounded to 10s — this is for cheap
// status checks, not builds) and returns its combined stdout+stderr as a
// string. Used by prereq-style checks that need real output, not just a
// streamed job log.
func Capture(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s failed: %w", name, err)
	}
	return string(out), nil
}

func commandLine(name string, args []string) string {
	s := name
	for _, a := range args {
		s += " " + a
	}
	return s
}

type ringBuffer struct {
	lines []string
	cap   int
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{cap: n} }

func (r *ringBuffer) add(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.cap {
		r.lines = r.lines[len(r.lines)-r.cap:]
	}
}

func (r *ringBuffer) String() string {
	s := ""
	for _, l := range r.lines {
		s += l + "\n"
	}
	return s
}
