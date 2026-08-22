package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type appVersionClient interface {
	GetAppVersion() ([]driver.AppVersion, error)
}

type appVersionResult struct {
	App     string `json:"app"`
	Version string `json:"version,omitempty"`
}

var appVersionCmd = &cobra.Command{
	Use:     "app-version",
	Aliases: []string{"app-versions"},
	Short:   "Show current 115 client app versions",
	Long:    "Query the 115 app-version service for the currently advertised client versions. This is remote service metadata and is distinct from '115driver version', which reports the local CLI build version.",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		versions, err := loadAppVersions(appVersionCommandClient())
		if err != nil {
			return &exitError{code: classifyNetworkError(err, output.ExitError), msg: fmt.Sprintf("Get 115 app versions failed: %v", err)}
		}
		printer.PrintSuccess(map[string]interface{}{"versions": versions, "count": len(versions)})
		printAppVersions(versions)
		return nil
	},
}

func appVersionCommandClient() appVersionClient {
	if client != nil {
		return client
	}
	opts := []driver.Option{driver.UA(driver.UA115Browser)}
	if debugMode {
		opts = append(opts, driver.WithDebug())
	}
	return driver.New(opts...)
}

func loadAppVersions(versionClient appVersionClient) ([]appVersionResult, error) {
	versions, err := versionClient.GetAppVersion()
	if err != nil {
		return nil, err
	}
	result := make([]appVersionResult, 0, len(versions))
	for _, version := range versions {
		result = append(result, appVersionResult{App: version.AppName, Version: version.Version})
	}
	return result, nil
}

func printAppVersions(versions []appVersionResult) {
	if jsonOutput {
		return
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "APP\tVERSION")
	for _, version := range versions {
		value := version.Version
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\n", version.App, value)
	}
	_ = writer.Flush()
}

func init() {
	rootCmd.AddCommand(appVersionCmd)
}
