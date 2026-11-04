package daemon

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/rpajarola/mirrortools/mirror"
)

// Executor defines the interface for running a mirror job.
type Executor interface {
	Run(ctx context.Context, job *Job, onLog func(stream, line string)) error
}

// LogFunc is called whenever a job emits output.
type LogFunc func(stream, line string)

// CommandExecutor runs jobs by invoking the mirror CLI binary as an isolated child process.
type CommandExecutor struct {
	BinaryPath string // path to "mirror" binary; if empty, looked up on PATH
}

// Run executes the job in a subprocess and streams its stdout/stderr.
func (e *CommandExecutor) Run(ctx context.Context, job *Job, onLog func(stream, line string)) error {
	bin := e.BinaryPath
	if bin == "" {
		bin = "mirror"
	}

	// Build arguments: [method] [-flag val ...] <source> <dest>
	var args []string
	if job.Method != "" {
		args = append(args, job.Method)
	}

	for k, v := range job.Flags {
		flagName := "-" + strings.TrimPrefix(k, "-")
		if v == "true" {
			args = append(args, flagName)
		} else {
			args = append(args, flagName, v)
		}
	}

	args = append(args, job.Source, job.DestDir)

	cmd := exec.CommandContext(ctx, bin, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", bin, err)
	}

	var wg sync.WaitGroup
	scanStream := func(r io.Reader, streamName string) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if onLog != nil {
				onLog(streamName, line)
			}
		}
	}

	wg.Add(2)
	go scanStream(stdout, "stdout")
	go scanStream(stderr, "stderr")

	wg.Wait()
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// InProcessExecutor executes the mirror method directly in-process. Useful for unit tests
// and lightweight embedded scenarios.
type InProcessExecutor struct {
	// CustomRunners allows injecting mock functions for tests.
	CustomRunners map[string]func(ctx context.Context, source, destDir string, flags map[string]string, onLog LogFunc) error
}

// Run executes the job in-process.
func (e *InProcessExecutor) Run(ctx context.Context, job *Job, onLog func(stream, line string)) error {
	if e != nil && e.CustomRunners != nil {
		if runner, ok := e.CustomRunners[job.Method]; ok {
			return runner(ctx, job.Source, job.DestDir, job.Flags, onLog)
		}
	}

	m, ok := mirror.Get(job.Method)
	if !ok {
		return fmt.Errorf("unknown mirror method %q", job.Method)
	}

	fs := flag.NewFlagSet(m.Name, flag.ContinueOnError)
	runFunc := m.SetupFlags(fs)

	for k, v := range job.Flags {
		flagName := strings.TrimPrefix(k, "-")
		if fs.Lookup(flagName) == nil {
			return fmt.Errorf("method %s has no flag -%s", m.Name, flagName)
		}
		if err := fs.Set(flagName, v); err != nil {
			return fmt.Errorf("setting flag -%s: %w", flagName, err)
		}
	}

	if onLog != nil {
		onLog("info", fmt.Sprintf("in-process start: %s %s -> %s", job.Method, job.Source, job.DestDir))
	}

	err := runFunc(ctx, job.Source, job.DestDir)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			if onLog != nil {
				onLog("info", "job cancelled by context")
			}
			return err
		}
		if onLog != nil {
			onLog("stderr", fmt.Sprintf("failed: %v", err))
		}
		return err
	}

	if onLog != nil {
		onLog("info", "completed successfully")
	}
	return nil
}
