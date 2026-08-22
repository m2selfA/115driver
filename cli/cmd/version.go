package cmd

import (
	"fmt"

	"github.com/SheltonZhu/115driver/internal/buildinfo"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("115driver version " + currentVersion())
	},
}

func currentVersion() string {
	return buildinfo.Version(version)
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
