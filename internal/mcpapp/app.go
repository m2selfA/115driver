package mcpapp

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/mcp/server"
	"github.com/SheltonZhu/115driver/mcp/server/tools"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

const ProgramName = "115driver-mcp-server"

// Main runs the MCP command-line entry point and exits with its status code.
func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run executes the MCP command-line entry point. It is separated from Main so
// the command contract can be tested without spawning a subprocess.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(ProgramName, flag.ContinueOnError)
	fs.SetOutput(stderr)

	cookie := fs.String("cookie", "", "115 driver cookie for authentication")
	profile := fs.String("profile", "", "Config profile name (default 'main')")
	configPath := fs.String("config", "", "Config file path (default ~/.115driver/config.toml)")
	localRoot := fs.String("local-root", "", "allow MCP local file tools to read/write only under this directory")
	urlUploadMaxBytes := fs.Int64(
		"url-upload-max-bytes",
		2<<30,
		"maximum bytes for upload_from_url downloads, use 0 to disable",
	)
	downloadMaxBytes := fs.Int64(
		"download-max-bytes",
		0,
		"maximum bytes per file for all MCP download tools, use 0 to disable",
	)
	allowSensitive := fs.Bool(
		"allow-sensitive-tools",
		false,
		"register MCP tools that return signed URLs or credential-like data",
	)
	allowDestructive := fs.Bool(
		"allow-destructive-tools",
		false,
		"register MCP tools that mutate 115 cloud state",
	)
	downloadTimeout := fs.Duration(
		"download-timeout",
		2*time.Hour,
		"timeout for MCP HTTP downloads and URL uploads, use 0 to disable",
	)
	showVersion := fs.Bool("version", false, "display version information")
	help := fs.Bool("help", false, "display help information")
	fs.Usage = func() { printHelp(fs, stderr) }

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	if *help {
		fs.Usage()
		return 0
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "Unexpected positional arguments: %v\n", fs.Args())
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", ProgramName, server.Version())
		return 0
	}

	if err := ValidateOptions(*urlUploadMaxBytes, *downloadMaxBytes, *downloadTimeout); err != nil {
		fmt.Fprintf(stderr, "Invalid options: %v\n", err)
		return 2
	}
	normalizedLocalRoot, err := tools.NormalizeLocalRoot(*localRoot)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid local root: %v\n", err)
		return 2
	}

	cookieStr := *cookie
	if cookieStr == "" {
		cookieStr = ReadConfigValue(*configPath, *profile, "cookie")
	}
	if cookieStr == "" {
		fmt.Fprintln(stderr, "Error: Cookie is required. Provide via --cookie or configure in ~/.115driver/config.toml (run '115driver login' first).")
		fmt.Fprintf(stderr, "Usage: %s --cookie=\"UID=xxx;CID=xxx;SEID=xxx\" [--profile main] [--config ~/.115driver/config.toml]\n", ProgramName)
		return 1
	}

	cr, err := CredentialFromCookie(cookieStr)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid cookie: %v\n", err)
		return 1
	}
	client := driver.New(driver.UA(driver.UA115Browser)).ImportCredential(cr)

	// CookieCheck is intentionally used here because LoginCheck can log out other
	// devices. Starting the MCP server must not have that account-side effect.
	if err := client.CookieCheck(); err != nil {
		fmt.Fprintf(stderr, "Authentication failed: %v\n", err)
		return 1
	}

	defaultSaveDir := ReadConfigValue(*configPath, *profile, "default_offline_save_dir")
	transferConfig, err := ReadDownloadTransferConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid transfer config: %v\n", err)
		return 1
	}

	var syncJournalStore *syncjournalpkg.Store
	if normalizedLocalRoot != "" {
		sessionConfig, err := ReadSessionStoreConfig(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "Invalid session store config: %v\n", err)
			return 1
		}
		_, profileScope, err := ResolveSessionScope(*configPath, *profile)
		if err != nil {
			fmt.Fprintf(stderr, "Invalid session profile scope: %v\n", err)
			return 1
		}
		// Store layout/scope are local configuration. Account binding is resolved
		// lazily through the read-only user-info endpoint by journal operations,
		// so merely enabling --local-root performs no extra account request.
		syncJournalStore = newSyncJournalStore(sessionConfig, profileScope)
	}

	s := server.NewServer().
		WithClient(client).
		WithDefaultSaveDir(defaultSaveDir).
		WithLocalRoot(normalizedLocalRoot).
		WithDownloadTimeout(*downloadTimeout).
		WithTransferSizeLimits(*urlUploadMaxBytes, *downloadMaxBytes).
		WithDownloadTransferConfig(transferConfig).
		WithSyncJournalStore(syncJournalStore).
		WithSensitiveTools(*allowSensitive).
		WithDestructiveTools(*allowDestructive)
	if err := s.Start(context.Background()); err != nil {
		fmt.Fprintf(stderr, "Server failed: %v\n", err)
		return 1
	}
	return 0
}

func printHelp(fs *flag.FlagSet, stderr io.Writer) {
	fmt.Fprintf(stderr, "Usage: %s [OPTIONS]\n", ProgramName)
	fmt.Fprintln(stderr, "115 Driver MCP Server - Provides access to 115 cloud storage via MCP protocol")
	fmt.Fprintln(stderr)
	fmt.Fprintln(stderr, "Options:")
	fs.PrintDefaults()
	fmt.Fprintln(stderr, "\nExamples:")
	fmt.Fprintf(stderr, "  With explicit cookie:   %s --cookie=\"UID=xxx;CID=xxx;SEID=xxx\"\n", ProgramName)
	fmt.Fprintf(stderr, "  From config file:       %s --profile main\n", ProgramName)
	fmt.Fprintln(stderr, "\nThe --cookie flag can be omitted if the config file (~/.115driver/config.toml)")
	fmt.Fprintln(stderr, "has a valid cookie under the specified profile. Run '115driver login' first.")
}
