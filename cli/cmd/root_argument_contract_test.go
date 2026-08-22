package cmd

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

func TestNormalizeArgumentErrorPreservesStructuredExitError(t *testing.T) {
	original := &exitError{code: output.ExitNotFound, msg: "missing"}
	if got := normalizeArgumentError(original); got != original {
		t.Fatalf("structured exit error was replaced: got %T %v", got, got)
	}
}

func TestNormalizeArgumentErrorMapsPlainValidationErrorToExitArgs(t *testing.T) {
	err := normalizeArgumentError(errors.New("bad arguments"))
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitArgs || ee.msg != "bad arguments" {
		t.Fatalf("normalized argument error = %T %#v", err, err)
	}
}

func TestInstallArgumentErrorContractRecurses(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	child := &cobra.Command{Use: "child <arg>", Args: cobra.ExactArgs(1)}
	grandchild := &cobra.Command{Use: "leaf", Args: cobra.NoArgs}
	child.AddCommand(grandchild)
	root.AddCommand(child)

	installArgumentErrorContract(root)
	for name, testCase := range map[string]struct {
		command *cobra.Command
		args    []string
	}{
		"child": {command: child, args: nil},
		"leaf":  {command: grandchild, args: []string{"unexpected"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := testCase.command.Args(testCase.command, testCase.args)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("wrapped validator error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}
