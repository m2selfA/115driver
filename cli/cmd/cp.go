package cmd

import (
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

var cpDryRun bool

var cpCmd = &cobra.Command{
	Use:   "cp <source_path>... <destination_dir>",
	Short: "Copy files or directories into a destination directory",
	Long:  "Copy one or more remote files/directories into destination_dir. Directory objects are copied recursively by 115; no --recursive flag is required. --dry-run resolves every source ID and destination mapping without submitting a copy request.",
	Args:  transferSourceArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		expandedArgs, err := expandTransferSourceArgs(cmd, args)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if cpDryRun {
			plan, err := buildMoveOrCopyPlan("copy", client, expandedArgs[:len(expandedArgs)-1], expandedArgs[len(expandedArgs)-1])
			if err != nil {
				return err
			}
			printer.PrintSuccess(plan)
			printMoveOrCopyPlan(plan)
			return nil
		}
		return moveOrCopy("copy", client, expandedArgs[:len(expandedArgs)-1], expandedArgs[len(expandedArgs)-1], client.Copy)
	},
}

func init() {
	addBatchFromFileFlag(cpCmd)
	cpCmd.Flags().BoolVar(&cpDryRun, "dry-run", false, "Plan and validate copies without changing remote data")
	rootCmd.AddCommand(cpCmd)
}
