package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/spf13/cobra"
)

type batchItemResult struct {
	Input   string      `json:"input"`
	Success bool        `json:"success"`
	Code    int         `json:"code,omitempty"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

var (
	parallelBatchActiveFlag          bool
	parallelBatchWorkersPerInterface int
)

func addContinueOnErrorFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("continue-on-error", false, "Continue processing remaining batch items after an item fails")
}

func addBatchJobsFlag(cmd *cobra.Command) {
	cmd.Flags().Int("jobs", 1, "Run up to N batch sources concurrently, sharing the workers-per-interface budget")
}

func addBatchFromFileFlag(cmd *cobra.Command) {
	cmd.Flags().String("from-file", "", "Read additional batch sources from a line-delimited file; use - for stdin")
}

func transferSourceArgs(cmd *cobra.Command, args []string) error {
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return err
	}
	if fromFile == "" {
		return cobra.MinimumNArgs(2)(cmd, args)
	}
	if len(args) < 1 {
		return fmt.Errorf("requires a destination argument when --from-file is used")
	}
	return nil
}

func batchInputArgs(cmd *cobra.Command, args []string) error {
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return err
	}
	if fromFile == "" {
		return cobra.MinimumNArgs(1)(cmd, args)
	}
	return nil
}

func batchFromFile(cmd *cobra.Command) (string, error) {
	if cmd == nil || cmd.Flags().Lookup("from-file") == nil {
		return "", nil
	}
	value, err := cmd.Flags().GetString("from-file")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func expandTransferSourceArgs(cmd *cobra.Command, args []string) ([]string, error) {
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return nil, err
	}
	if fromFile == "" {
		return append([]string(nil), args...), nil
	}
	if len(args) < 1 {
		return nil, fmt.Errorf("requires a destination argument when --from-file is used")
	}
	destination := args[len(args)-1]
	sources := append([]string(nil), args[:len(args)-1]...)
	fileSources, err := readBatchSources(cmd, fromFile)
	if err != nil {
		return nil, err
	}
	sources = append(sources, fileSources...)
	if len(sources) == 0 {
		return nil, fmt.Errorf("--from-file %q did not provide any sources", fromFile)
	}
	return append(sources, destination), nil
}

func expandBatchInputArgs(cmd *cobra.Command, args []string) ([]string, error) {
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return nil, err
	}
	inputs := append([]string(nil), args...)
	if fromFile != "" {
		fileInputs, err := readBatchSources(cmd, fromFile)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, fileInputs...)
	}
	if len(inputs) == 0 {
		if fromFile != "" {
			return nil, fmt.Errorf("--from-file %q did not provide any inputs", fromFile)
		}
		return nil, fmt.Errorf("at least one input is required")
	}
	return inputs, nil
}

const maxBatchSourceLineBytes = 1024 * 1024

func readBatchSources(cmd *cobra.Command, fromFile string) ([]string, error) {
	var reader io.Reader
	var closeReader io.Closer
	if fromFile == "-" {
		if cmd == nil {
			reader = os.Stdin
		} else {
			reader = cmd.InOrStdin()
		}
	} else {
		file, err := os.Open(fromFile)
		if err != nil {
			return nil, fmt.Errorf("read batch source file %q: %w", fromFile, err)
		}
		reader = file
		closeReader = file
	}
	if closeReader != nil {
		defer closeReader.Close()
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxBatchSourceLineBytes)
	sources := make([]string, 0)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		value := scanner.Text()
		if lineNumber == 1 {
			value = strings.TrimPrefix(value, "\ufeff")
		}
		if value == "" {
			continue
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("batch source %q line %d contains a NUL byte", fromFile, lineNumber)
		}
		sources = append(sources, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read batch source file %q: %w", fromFile, err)
	}
	return sources, nil
}

func batchJobs(cmd *cobra.Command) (int, error) {
	if cmd == nil || cmd.Flags().Lookup("jobs") == nil {
		return 1, nil
	}
	value, err := cmd.Flags().GetInt("jobs")
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("--jobs must be > 0")
	}
	return value, nil
}

func batchContinueOnError(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	value, err := cmd.Flags().GetBool("continue-on-error")
	return err == nil && value
}

func validateStaticTransferBatchPolicy(cmd *cobra.Command, args []string, jobs int, operationPlural string) error {
	if jobs <= 1 {
		return nil
	}
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return err
	}
	explicitSources := len(args) - 1
	if fromFile == "" && explicitSources <= 1 {
		return fmt.Errorf("--jobs > 1 is only valid for multi-source %s", operationPlural)
	}
	if explicitSources > 1 && !batchContinueOnError(cmd) {
		return fmt.Errorf("--jobs > 1 requires --continue-on-error so concurrent failure semantics remain explicit")
	}
	return nil
}

func successfulBatchItem(input string, data interface{}) batchItemResult {
	return batchItemResult{Input: input, Success: true, Data: data}
}

func failedBatchItem(input string, data interface{}, err error) batchItemResult {
	return batchItemResult{Input: input, Success: false, Code: commandErrorCode(err), Error: err.Error(), Data: data}
}

func commandErrorCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) && ee.code != 0 {
		return ee.code
	}
	return output.ExitError
}

func batchResultData(total int, items []batchItemResult, base map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = make(map[string]interface{})
	}
	succeeded := 0
	failed := 0
	for _, item := range items {
		if item.Success {
			succeeded++
		} else {
			failed++
		}
	}
	base["count"] = total
	base["processed"] = len(items)
	base["succeeded"] = succeeded
	base["failed"] = failed
	base["remaining"] = total - len(items)
	base["items"] = items
	return base
}

func batchFailedCount(items []batchItemResult) int {
	failed := 0
	for _, item := range items {
		if !item.Success {
			failed++
		}
	}
	return failed
}

func batchFailureExitCode(items []batchItemResult) int {
	code := 0
	for _, item := range items {
		if item.Success {
			continue
		}
		itemCode := item.Code
		if itemCode == 0 {
			itemCode = output.ExitError
		}
		if code == 0 {
			code = itemCode
			continue
		}
		if code != itemCode {
			return output.ExitError
		}
	}
	if code == 0 {
		return output.ExitError
	}
	return code
}

func batchIncompleteError(operation string, total int, items []batchItemResult, data map[string]interface{}) error {
	failed := batchFailedCount(items)
	remaining := total - len(items)
	message := fmt.Sprintf("%s incomplete: %d item(s) failed", operation, failed)
	if remaining > 0 {
		message += fmt.Sprintf(", %d item(s) not processed", remaining)
	}
	return &exitError{code: batchFailureExitCode(items), msg: message, data: data}
}

func batchParallelActive() bool {
	return parallelBatchActiveFlag
}

func applyParallelBatchWorkerLimit(workers int) int {
	if parallelBatchWorkersPerInterface > 0 && workers > parallelBatchWorkersPerInterface {
		return parallelBatchWorkersPerInterface
	}
	return workers
}

func runParallelBatch(total, jobs, workersPerInterface int, run func(int) error) []error {
	results := make([]error, total)
	if total == 0 {
		return results
	}
	if jobs > total {
		jobs = total
	}
	if jobs < 1 {
		jobs = 1
	}

	oldActive := parallelBatchActiveFlag
	oldWorkers := parallelBatchWorkersPerInterface
	oldPrinter := printer
	parallelBatchActiveFlag = true
	parallelBatchWorkersPerInterface = workersPerInterface
	printer = output.NewPrinter(false)
	defer func() {
		printer = oldPrinter
		parallelBatchWorkersPerInterface = oldWorkers
		parallelBatchActiveFlag = oldActive
	}()

	indices := make(chan int)
	var wait sync.WaitGroup
	wait.Add(jobs)
	for worker := 0; worker < jobs; worker++ {
		go func() {
			defer wait.Done()
			for index := range indices {
				results[index] = run(index)
			}
		}()
	}
	for index := 0; index < total; index++ {
		indices <- index
	}
	close(indices)
	wait.Wait()
	return results
}

func runBatchItem(run func() error) error {
	originalPrinter := printer
	if jsonOutput {
		printer = output.NewPrinter(false)
	}
	defer func() { printer = originalPrinter }()
	return run()
}

func printBatchItemFailure(index, total int, label string, err error) {
	if !jsonOutput {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s failed: %v\n", index+1, total, label, err)
	}
}
