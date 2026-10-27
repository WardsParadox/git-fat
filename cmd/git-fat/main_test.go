package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

var allCommands = []string{"init", "status", "push", "pull", "gc", "verify", "checkout", "find",
	"index-filter", "profile", "s3-info", "version", "filter-clean", "filter-smudge"}

// Building every flag set catches "flag redefined" panics.
func TestEveryCommandBuildsItsFlagSet(t *testing.T) {
	for _, name := range []string{"status", "push", "pull", "s3-info"} {
		fs, tf := newFlags(name)
		if _, _, _, err := parseArgs(fs, []string{"--all", "-j", "2", "--part-size", "6", "--no-accelerate"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !*tf.all || *tf.jobs != 2 || *tf.partMB != 6 || !*tf.noAccel {
			t.Errorf("%s: flags not applied", name)
		}
	}
	newProfileFlags()
}

// --help must work for every command without a repository or a panic.
func TestHelpForEveryCommand(t *testing.T) {
	for _, cmd := range allCommands {
		err := run(context.Background(), []string{cmd, "--help"})
		if err != nil && !errors.Is(err, errHelp) {
			t.Errorf("%s --help: %v", cmd, err)
		}
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	for _, cmd := range []string{"status", "push", "pull", "gc", "verify", "checkout", "init", "find", "profile", "s3-info", "index-filter"} {
		err := run(context.Background(), []string{cmd, "--no-such-flag"})
		if err != exitError(2) {
			t.Errorf("%s: err = %v, want exit 2", cmd, err)
		}
	}
	if err := run(context.Background(), []string{"bogus"}); err != exitError(2) {
		t.Errorf("unknown command: %v", err)
	}
}

func TestInterspersedFlags(t *testing.T) {
	fs, tf := newFlags("pull")
	pos, after, has, err := parseArgs(fs, []string{"abc123", "--all", "-j", "4", "--", "*.bin", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"abc123"}) || !*tf.all || *tf.jobs != 4 {
		t.Errorf("pos=%v all=%v jobs=%d", pos, *tf.all, *tf.jobs)
	}
	if !has || !reflect.DeepEqual(after, []string{"*.bin", "-x"}) {
		t.Errorf("after=%v has=%v", after, has)
	}
}
