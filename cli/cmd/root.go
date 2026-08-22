package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	cookieFlag string
	configPath string
	profile    string
	jsonOutput bool
	debugMode  bool
)

// Set via -ldflags at build time
var version = "dev"

var client *driver.Pan115Client
var printer *output.Printer

var argumentErrorContractOnce sync.Once

func normalizeArgumentError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	return &exitError{code: output.ExitArgs, msg: err.Error()}
}

func installArgumentErrorContract(command *cobra.Command) {
	if command == nil {
		return
	}
	if command.Args != nil {
		validator := command.Args
		command.Args = func(cmd *cobra.Command, args []string) error {
			return normalizeArgumentError(validator(cmd, args))
		}
	}
	for _, child := range command.Commands() {
		installArgumentErrorContract(child)
	}
}

func validatePureCommandGroupInvocation(root *cobra.Command, args []string) error {
	if root == nil {
		return nil
	}
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	command, remaining, err := root.Find(args)
	if err != nil {
		return normalizeArgumentError(err)
	}
	if command == nil || command.Run != nil || command.RunE != nil || !command.HasSubCommands() {
		return nil
	}

	command.InitDefaultHelpFlag()
	if err := command.ParseFlags(remaining); err != nil {
		return normalizeArgumentError(err)
	}
	if help, err := command.Flags().GetBool("help"); err == nil && help {
		return nil
	}
	if command.Args == nil {
		return nil
	}
	return normalizeArgumentError(command.Args(command, command.Flags().Args()))
}

func commandSkipsAuthentication(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	name := cmd.Name()
	switch name {
	case "login", "help", "completion", "version", "app-version", "config", "sessions", "__complete", "__completeNoDesc":
		return true
	}
	parent := cmd.Parent()
	if parent == nil {
		return false
	}
	pname := parent.Name()
	if pname == "config" || pname == "completion" {
		return true
	}
	if pname == "sessions" && !(name == "rm" && sessionsRmAbortRemote) {
		return true
	}
	if pname == "journal" {
		if grandparent := parent.Parent(); grandparent != nil && grandparent.Name() == "sync" {
			return name != "verify" && name != "recover"
		}
	}
	if pname == "trash" || pname == "aliases" {
		if grandparent := parent.Parent(); grandparent != nil && grandparent.Name() == "journal" {
			if greatGrandparent := grandparent.Parent(); greatGrandparent != nil && greatGrandparent.Name() == "sync" {
				return true
			}
		}
	}
	return false
}

var rootCmd = &cobra.Command{
	Use:           "115driver",
	Short:         "CLI tool for 115 cloud storage",
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		printer = output.NewPrinter(jsonOutput)

		if commandSkipsAuthentication(cmd) {
			return nil
		}

		cr, err := auth.ResolveCredential(cookieFlag, configPath, profile)
		if err != nil {
			return &exitError{code: output.ExitAuth, msg: err.Error()}
		}

		opts := []driver.Option{driver.UA(driver.UA115Browser)}
		if debugMode {
			opts = append(opts, driver.WithDebug())
		}
		client = driver.New(opts...).ImportCredential(cr)

		user, err := client.GetUser()
		if err != nil {
			code := classifyNetworkError(err, output.ExitAuth)
			message := fmt.Sprintf("Authentication failed: %v\nRun '115driver login' to re-authenticate.", err)
			if code == output.ExitNetwork {
				message = fmt.Sprintf("Authentication check failed due to network error: %v", err)
			}
			return &exitError{code: code, msg: message}
		}
		if user != nil {
			client.UserID = user.UserID
		}
		return nil
	},
}

type exitError struct {
	code int
	msg  string
	data interface{}
}

func (e *exitError) Error() string {
	return e.msg
}

func init() {
	rootCmd.Version = currentVersion()
	rootCmd.PersistentFlags().StringVar(&cookieFlag, "cookie", "", "Cookie string (or set DRIVER115_COOKIE env)")
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "Config file path (default ~/.115driver/config.toml)")
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "Config profile name (default 'main')")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format (for AI agents)")
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Enable debug mode")
	rootCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return normalizeArgumentError(err)
	})
	rootCmd.CompletionOptions.HiddenDefaultCmd = true
}

func printCommandError(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.data != nil {
			return printer.PrintErrorData(ee.msg, ee.code, ee.data)
		}
		return printer.PrintError(ee.msg, ee.code)
	}
	return printer.PrintError(err.Error(), output.ExitError)
}

func Execute() int {
	rootCmd.InitDefaultHelpCmd()
	argumentErrorContractOnce.Do(func() {
		installArgumentErrorContract(rootCmd)
	})
	// Pre-scan os.Args for --json so printer is initialized correctly
	// even when flag parsing fails early (unknown command, etc.)
	for _, arg := range os.Args[1:] {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			jsonOutput = true
			break
		}
	}
	if printer == nil {
		printer = output.NewPrinter(jsonOutput)
	}

	if err := validatePureCommandGroupInvocation(rootCmd, os.Args[1:]); err != nil {
		return printCommandError(err)
	}
	if err := rootCmd.Execute(); err != nil {
		return printCommandError(err)
	}
	return output.ExitSuccess
}
