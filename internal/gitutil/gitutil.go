// Package gitutil wraps the git command line.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Command returns a git command bound to ctx.
func Command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...)
}

// Output runs git and returns its stdout. On failure the error carries stderr.
func Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := Command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// Run runs git, discarding stdout.
func Run(ctx context.Context, args ...string) error {
	_, err := Output(ctx, args...)
	return err
}

// ConfigGet returns the value of a config key. When file is non-empty the
// file is consulted first, then the repository configuration.
func ConfigGet(ctx context.Context, name, file string) (string, bool) {
	if file != "" {
		if v, ok := configGet(ctx, "--file", file, name); ok {
			return v, true
		}
	}
	return configGet(ctx, name)
}

func configGet(ctx context.Context, args ...string) (string, bool) {
	out, err := Output(ctx, append([]string{"config", "--get"}, args...)...)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// ConfigSet writes a config key to the repository config, or to file if given.
func ConfigSet(ctx context.Context, name, value, file string) error {
	args := []string{"config"}
	if file != "" {
		args = append(args, "--file", file)
	}
	return Run(ctx, append(args, name, value)...)
}

// RevParse resolves a revision to an object name.
func RevParse(ctx context.Context, rev string) (string, error) {
	out, err := Output(ctx, "rev-parse", rev)
	return strings.TrimSpace(string(out)), err
}
