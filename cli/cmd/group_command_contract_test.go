package cmd

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func TestPureCommandGroupsRejectResidualArguments(t *testing.T) {
	for name, command := range map[string]*cobra.Command{
		"offline":      offlineCmd,
		"recycle":      recycleCmd,
		"sessions":     sessionsCmd,
		"share":        shareCmd,
		"sync journal": syncJournalCmd,
	} {
		t.Run(name, func(t *testing.T) {
			if command.Args == nil {
				t.Fatal("pure command group has no argument validator")
			}
			err := normalizeArgumentError(command.Args(command, []string{"definitely-not-a-command"}))
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("residual argument error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestValidatePureCommandGroupInvocationRejectsUnknownChild(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().String("config", "", "config path")
	group := &cobra.Command{Use: "group", Args: cobra.NoArgs}
	group.AddCommand(&cobra.Command{Use: "known", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(group)

	for name, args := range map[string][]string{
		"plain":             {"group", "definitely-not-a-command"},
		"persistent-before": {"--config", "cfg.toml", "group", "definitely-not-a-command"},
		"persistent-after":  {"group", "--config", "cfg.toml", "definitely-not-a-command"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validatePureCommandGroupInvocation(root, args)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("group preflight error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestValidatePureCommandGroupInvocationAllowsLazyVersionFlag(t *testing.T) {
	root := &cobra.Command{Use: "root", Version: "1.2.3"}
	root.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	if err := validatePureCommandGroupInvocation(root, []string{"--version"}); err != nil {
		t.Fatalf("lazy root version flag rejected: %v", err)
	}
	flag := root.Flags().Lookup("version")
	if flag == nil {
		t.Fatal("version preflight did not initialize Cobra's lazy version flag")
	}
}

func TestValidatePureCommandGroupInvocationAllowsHelpAndRunnableParents(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	group := &cobra.Command{Use: "group", Args: cobra.NoArgs}
	group.AddCommand(&cobra.Command{Use: "known", Run: func(*cobra.Command, []string) {}})
	runnable := &cobra.Command{Use: "run <left> <right>", Args: cobra.ExactArgs(2), Run: func(*cobra.Command, []string) {}}
	runnable.AddCommand(&cobra.Command{Use: "nested", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(group, runnable)

	if err := validatePureCommandGroupInvocation(root, []string{"group", "--help"}); err != nil {
		t.Fatalf("group help rejected: %v", err)
	}
	if err := validatePureCommandGroupInvocation(root, []string{"run", "local", "remote"}); err != nil {
		t.Fatalf("runnable parent positional args rejected: %v", err)
	}
}
