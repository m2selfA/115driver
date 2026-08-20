package cmd

import (
	"github.com/spf13/cobra"
)

var cpCmd = &cobra.Command{
	Use:   "cp <source_path>... <destination_dir>",
	Short: "Copy files or directories into a destination directory",
	Long:  "Copy one or more remote files/directories into destination_dir. Directory objects are copied recursively by 115; no --recursive flag is required.",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return moveOrCopy("copy", client, args[:len(args)-1], args[len(args)-1], client.Copy)
	},
}

func init() {
	rootCmd.AddCommand(cpCmd)
}
