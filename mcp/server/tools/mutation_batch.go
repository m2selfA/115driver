package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPMutationBatchItems = maxMCPFileBatchItems
	maxMCPMutationTreeDepth  = 256
)

// MCPMutationObject is the read-only metadata snapshot used by mutation plans.
type MCPMutationObject struct {
	Index       int    `json:"index" jsonschema:"zero-based input item index"`
	FileID      string `json:"file_id" jsonschema:"115 file or directory ID"`
	ParentID    string `json:"parent_id,omitempty" jsonschema:"parent directory ID when available"`
	Name        string `json:"name,omitempty" jsonschema:"current remote object name"`
	IsDirectory bool   `json:"is_directory" jsonschema:"whether the object is a directory"`
}

// MCPMutationPlan is returned by dry-run variants of the existing mutation tools.
type MCPMutationPlan struct {
	Operation   string              `json:"operation" jsonschema:"planned mutation operation"`
	DryRun      bool                `json:"dry_run" jsonschema:"always true for a mutation preview"`
	Requested   int                 `json:"requested" jsonschema:"number of requested source objects"`
	TargetDirID string              `json:"target_dir_id,omitempty" jsonschema:"target directory ID for move/copy operations"`
	Items       []MCPMutationObject `json:"items,omitempty" jsonschema:"preflighted source objects in input order"`
}

// MkdirManyItem defines one directory creation in a homogeneous batch.
type MkdirManyItem struct {
	ParentID string `json:"parent_id" jsonschema:"parent directory ID; use 0 for root"`
	Name     string `json:"name" jsonschema:"name of the directory to create"`
}

// MkdirManyArgs creates multiple directories only after full-batch parent preflight.
type MkdirManyArgs struct {
	Directories     []MkdirManyItem `json:"directories" jsonschema:"directories to create after full-batch preflight"`
	DryRun          bool            `json:"dry_run,omitempty" jsonschema:"preview the entire batch without changing 115 state"`
	ContinueOnError bool            `json:"continue_on_error,omitempty" jsonschema:"continue later creates after a runtime failure; ignored during dry-run"`
}

// RenameManyItem defines one rename in a homogeneous batch.
type RenameManyItem struct {
	FileID  string `json:"file_id" jsonschema:"ID of file or directory to rename"`
	NewName string `json:"new_name" jsonschema:"new single-component remote name"`
}

// RenameManyArgs renames multiple objects only after full-batch source/name preflight.
type RenameManyArgs struct {
	Files           []RenameManyItem `json:"files" jsonschema:"objects to rename after full-batch preflight"`
	DryRun          bool             `json:"dry_run,omitempty" jsonschema:"preview the entire batch without changing 115 state"`
	ContinueOnError bool             `json:"continue_on_error,omitempty" jsonschema:"continue later renames after a runtime failure; ignored during dry-run"`
}

// MCPMutationBatchItemResult reports one mkdir_many/rename_many item.
type MCPMutationBatchItemResult struct {
	Index       int    `json:"index" jsonschema:"zero-based input item index"`
	FileID      string `json:"file_id,omitempty" jsonschema:"source object ID for rename or created directory ID after mkdir"`
	ParentID    string `json:"parent_id,omitempty" jsonschema:"parent directory ID"`
	Name        string `json:"name,omitempty" jsonschema:"current or created name"`
	NewName     string `json:"new_name,omitempty" jsonschema:"requested new name for rename"`
	IsDirectory bool   `json:"is_directory,omitempty" jsonschema:"whether the source object is a directory"`
	Status      string `json:"status" jsonschema:"planned, succeeded, failed, or not_run"`
	Error       string `json:"error,omitempty" jsonschema:"runtime item error"`
}

// MCPMutationBatchResult summarizes a homogeneous sequential mutation batch.
type MCPMutationBatchResult struct {
	Operation string                       `json:"operation" jsonschema:"mutation operation"`
	DryRun    bool                         `json:"dry_run" jsonschema:"whether no mutation was submitted"`
	Requested int                          `json:"requested" jsonschema:"number of requested items"`
	Planned   int                          `json:"planned" jsonschema:"number of items that passed full-batch preflight"`
	Succeeded int                          `json:"succeeded" jsonschema:"number of runtime successes"`
	Failed    int                          `json:"failed" jsonschema:"number of runtime failures"`
	Remaining int                          `json:"remaining" jsonschema:"number of preflighted items not run after a stop-on-error failure"`
	Items     []MCPMutationBatchItemResult `json:"items" jsonschema:"per-item status in input order"`
}

func validateMCPRemoteObjectName(name string) (string, error) {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." {
		return "", errors.New("remote name must be a non-empty single path component")
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, '\x00') {
		return "", errors.New("remote name must not contain path separators or NUL")
	}
	return name, nil
}

func normalizeMCPMutationFileID(fileID string) (string, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return "", errors.New("file_id must be non-empty")
	}
	if fileID == "0" {
		return "", errors.New("the 115 root directory cannot be used as a mutation source")
	}
	return fileID, nil
}

func normalizeMCPUniqueStrings(values []string, label string, maxItems int) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one %s is required", label)
	}
	if maxItems > 0 && len(values) > maxItems {
		return nil, fmt.Errorf("received %d %s values; maximum is %d", len(values), label, maxItems)
	}
	normalized := make([]string, len(values))
	seen := make(map[string]int, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s at index %d is empty", label, i)
		}
		if previous, ok := seen[value]; ok {
			return nil, fmt.Errorf("%s values at indexes %d and %d are duplicates", label, previous, i)
		}
		seen[value] = i
		normalized[i] = value
	}
	return normalized, nil
}

func mcpJSONResult(value any, failurePrefix string) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return toolError(fmt.Sprintf("%s: %v", failurePrefix, err)), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, value, nil
}

func loadMCPRemoteDirectory(client *driver.Pan115Client, dirID string) (*driver.File, error) {
	dirID, err := normalizeMCPUploadDirID(dirID)
	if err != nil {
		return nil, err
	}
	if dirID == "0" {
		return &driver.File{FileID: "0", Name: "/", IsDirectory: true}, nil
	}
	if client == nil {
		return nil, errors.New("115 client is unavailable")
	}
	entry, err := client.GetFile(dirID)
	if err != nil {
		return nil, fmt.Errorf("resolve target directory %q: %w", dirID, err)
	}
	if entry == nil || !entry.IsDirectory {
		return nil, fmt.Errorf("target id %q is not a directory", dirID)
	}
	return entry, nil
}

type mcpRemoteNameTarget struct {
	Index       int
	ParentID    string
	Name        string
	AllowFileID string
}

func preflightMCPRemoteTargetNames(client *driver.Pan115Client, targets []mcpRemoteNameTarget) error {
	if client == nil {
		return errors.New("115 client is unavailable")
	}
	grouped := make(map[string][]mcpRemoteNameTarget)
	parentOrder := make([]string, 0)
	for _, target := range targets {
		parentID, err := normalizeMCPUploadDirID(target.ParentID)
		if err != nil {
			return fmt.Errorf("item %d parent: %w", target.Index, err)
		}
		if _, exists := grouped[parentID]; !exists {
			parentOrder = append(parentOrder, parentID)
		}
		target.ParentID = parentID
		grouped[parentID] = append(grouped[parentID], target)
	}
	for _, parentID := range parentOrder {
		if _, err := loadMCPRemoteDirectory(client, parentID); err != nil {
			return err
		}
		entries, err := client.List(parentID, driver.WithRecordOpenTime(false))
		if err != nil {
			return fmt.Errorf("list parent directory %q for name preflight: %w", parentID, err)
		}
		if entries == nil {
			return fmt.Errorf("list parent directory %q for name preflight returned no response", parentID)
		}
		for _, target := range grouped[parentID] {
			for _, entry := range *entries {
				if entry.Name != target.Name {
					continue
				}
				if target.AllowFileID != "" && entry.FileID == target.AllowFileID {
					continue
				}
				return fmt.Errorf("item %d target name %q already exists in parent %q as object %q", target.Index, target.Name, parentID, entry.FileID)
			}
		}
	}
	return nil
}

func preflightMCPMutationObjects(client *driver.Pan115Client, fileIDs []string) ([]MCPMutationObject, []string, error) {
	if len(fileIDs) == 0 {
		return nil, nil, errors.New("at least one file_id is required")
	}
	if len(fileIDs) > maxMCPMutationBatchItems {
		return nil, nil, fmt.Errorf("mutation has %d source objects; maximum is %d", len(fileIDs), maxMCPMutationBatchItems)
	}
	if client == nil {
		return nil, nil, errors.New("115 client is unavailable")
	}

	objects := make([]MCPMutationObject, len(fileIDs))
	normalized := make([]string, len(fileIDs))
	seen := make(map[string]int, len(fileIDs))
	for i, rawID := range fileIDs {
		fileID, err := normalizeMCPMutationFileID(rawID)
		if err != nil {
			return nil, nil, fmt.Errorf("source %d: %w", i, err)
		}
		if previous, ok := seen[fileID]; ok {
			return nil, nil, fmt.Errorf("sources %d and %d use the same file_id %q", previous, i, fileID)
		}
		seen[fileID] = i
		entry, err := client.GetFile(fileID)
		if err != nil {
			return nil, nil, fmt.Errorf("preflight source %d (%s): %w", i, fileID, err)
		}
		if entry == nil {
			return nil, nil, fmt.Errorf("preflight source %d (%s) returned no object", i, fileID)
		}
		normalized[i] = fileID
		objects[i] = MCPMutationObject{
			Index:       i,
			FileID:      fileID,
			ParentID:    strings.TrimSpace(entry.ParentID),
			Name:        entry.Name,
			IsDirectory: entry.IsDirectory,
		}
	}
	return objects, normalized, nil
}

func validateMCPMoveCopyTargetAncestry(client *driver.Pan115Client, target *driver.File, sources []MCPMutationObject) error {
	if target == nil {
		return errors.New("target directory metadata is missing")
	}
	sourceDirs := make(map[string]struct{})
	for _, source := range sources {
		if source.IsDirectory {
			sourceDirs[source.FileID] = struct{}{}
		}
	}
	if len(sourceDirs) == 0 {
		return nil
	}

	current := target
	seen := make(map[string]struct{}, maxMCPMutationTreeDepth)
	for depth := 0; depth < maxMCPMutationTreeDepth; depth++ {
		currentID := strings.TrimSpace(current.FileID)
		if currentID == "" {
			return errors.New("target ancestry contains an object without an id")
		}
		if _, forbidden := sourceDirs[currentID]; forbidden {
			return fmt.Errorf("target directory %q is the same as or inside source directory %q", target.FileID, currentID)
		}
		if currentID == "0" {
			return nil
		}
		if _, duplicate := seen[currentID]; duplicate {
			return fmt.Errorf("target ancestry contains a cycle at %q", currentID)
		}
		seen[currentID] = struct{}{}

		parentID := strings.TrimSpace(current.ParentID)
		if parentID == "" {
			return fmt.Errorf("target directory %q has no parent identity", currentID)
		}
		if parentID == "0" {
			return nil
		}
		parent, err := loadMCPRemoteDirectory(client, parentID)
		if err != nil {
			return fmt.Errorf("resolve target ancestry parent %q: %w", parentID, err)
		}
		current = parent
	}
	return fmt.Errorf("target ancestry exceeds %d directories", maxMCPMutationTreeDepth)
}

func mutationPlanCallResult(plan MCPMutationPlan) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize mutation dry-run: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, plan, nil
}

func mutationBatchCallResult(result MCPMutationBatchResult) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize mutation batch result: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		IsError: result.Failed > 0,
	}, result, nil
}

func (ft *FileTools) mkdirMany(ctx context.Context, req *mcp.CallToolRequest, args MkdirManyArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Directories) == 0 {
		return toolError("mkdir_many requires at least one directory"), nil, nil
	}
	if len(args.Directories) > maxMCPMutationBatchItems {
		return toolError(fmt.Sprintf("mkdir_many has %d directories; maximum is %d", len(args.Directories), maxMCPMutationBatchItems)), nil, nil
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}

	prepared := make([]MkdirManyItem, len(args.Directories))
	seenTargets := make(map[string]int, len(args.Directories))
	nameTargets := make([]mcpRemoteNameTarget, len(args.Directories))
	for i, item := range args.Directories {
		parentID, err := normalizeMCPUploadDirID(item.ParentID)
		if err != nil {
			return toolError(fmt.Sprintf("mkdir_many item %d parent preflight failed: %v", i, err)), nil, nil
		}
		name, err := validateMCPRemoteObjectName(item.Name)
		if err != nil {
			return toolError(fmt.Sprintf("mkdir_many item %d name preflight failed: %v", i, err)), nil, nil
		}
		key := parentID + "\x00" + name
		if previous, ok := seenTargets[key]; ok {
			return toolError(fmt.Sprintf("mkdir_many items %d and %d target the same parent/name", previous, i)), nil, nil
		}
		seenTargets[key] = i
		prepared[i] = MkdirManyItem{ParentID: parentID, Name: name}
		nameTargets[i] = mcpRemoteNameTarget{Index: i, ParentID: parentID, Name: name}
	}
	if err := preflightMCPRemoteTargetNames(ft.client, nameTargets); err != nil {
		return toolError(fmt.Sprintf("mkdir_many remote name preflight failed: %v", err)), nil, nil
	}

	result := MCPMutationBatchResult{
		Operation: "mkdir_many", DryRun: args.DryRun, Requested: len(prepared), Planned: len(prepared),
		Items: make([]MCPMutationBatchItemResult, len(prepared)),
	}
	for i, item := range prepared {
		result.Items[i] = MCPMutationBatchItemResult{Index: i, ParentID: item.ParentID, Name: item.Name, IsDirectory: true, Status: "planned"}
	}
	if args.DryRun {
		return mutationBatchCallResult(result)
	}

	for i, item := range prepared {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				result.Items[i].Status = "failed"
				result.Items[i].Error = err.Error()
				result.Failed++
				if !args.ContinueOnError {
					for j := i + 1; j < len(prepared); j++ {
						result.Items[j].Status = "not_run"
					}
					result.Remaining = len(prepared) - i - 1
					break
				}
				continue
			}
		}
		dirID, err := ft.client.Mkdir(item.ParentID, item.Name)
		if err != nil {
			result.Items[i].Status = "failed"
			result.Items[i].Error = err.Error()
			result.Failed++
			if !args.ContinueOnError {
				for j := i + 1; j < len(prepared); j++ {
					result.Items[j].Status = "not_run"
				}
				result.Remaining = len(prepared) - i - 1
				break
			}
			continue
		}
		result.Items[i].FileID = dirID
		result.Items[i].Status = "succeeded"
		result.Succeeded++
	}
	return mutationBatchCallResult(result)
}

func (ft *FileTools) renameMany(ctx context.Context, req *mcp.CallToolRequest, args RenameManyArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Files) == 0 {
		return toolError("rename_many requires at least one file"), nil, nil
	}
	if len(args.Files) > maxMCPMutationBatchItems {
		return toolError(fmt.Sprintf("rename_many has %d files; maximum is %d", len(args.Files), maxMCPMutationBatchItems)), nil, nil
	}

	ids := make([]string, len(args.Files))
	newNames := make([]string, len(args.Files))
	for i, item := range args.Files {
		ids[i] = item.FileID
		name, err := validateMCPRemoteObjectName(item.NewName)
		if err != nil {
			return toolError(fmt.Sprintf("rename_many item %d name preflight failed: %v", i, err)), nil, nil
		}
		newNames[i] = name
	}
	objects, normalizedIDs, err := preflightMCPMutationObjects(ft.client, ids)
	if err != nil {
		return toolError(fmt.Sprintf("rename_many source preflight failed: %v", err)), nil, nil
	}
	seenTargets := make(map[string]int, len(objects))
	currentNames := make(map[string]int, len(objects))
	for i, object := range objects {
		currentNames[object.ParentID+"\x00"+object.Name] = i
		key := object.ParentID + "\x00" + newNames[i]
		if previous, ok := seenTargets[key]; ok {
			return toolError(fmt.Sprintf("rename_many items %d and %d target the same parent/name", previous, i)), nil, nil
		}
		seenTargets[key] = i
	}
	for i, object := range objects {
		key := object.ParentID + "\x00" + newNames[i]
		if occupant, ok := currentNames[key]; ok && occupant != i {
			return toolError(fmt.Sprintf("rename_many item %d targets the current name of batch item %d; sequential rename would be order-dependent", i, occupant)), nil, nil
		}
	}
	nameTargets := make([]mcpRemoteNameTarget, len(objects))
	for i, object := range objects {
		nameTargets[i] = mcpRemoteNameTarget{Index: i, ParentID: object.ParentID, Name: newNames[i], AllowFileID: object.FileID}
	}
	if err := preflightMCPRemoteTargetNames(ft.client, nameTargets); err != nil {
		return toolError(fmt.Sprintf("rename_many remote name preflight failed: %v", err)), nil, nil
	}

	result := MCPMutationBatchResult{
		Operation: "rename_many", DryRun: args.DryRun, Requested: len(objects), Planned: len(objects),
		Items: make([]MCPMutationBatchItemResult, len(objects)),
	}
	for i, object := range objects {
		result.Items[i] = MCPMutationBatchItemResult{
			Index: i, FileID: object.FileID, ParentID: object.ParentID, Name: object.Name,
			NewName: newNames[i], IsDirectory: object.IsDirectory, Status: "planned",
		}
	}
	if args.DryRun {
		return mutationBatchCallResult(result)
	}

	for i := range objects {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				result.Items[i].Status = "failed"
				result.Items[i].Error = err.Error()
				result.Failed++
				if !args.ContinueOnError {
					for j := i + 1; j < len(objects); j++ {
						result.Items[j].Status = "not_run"
					}
					result.Remaining = len(objects) - i - 1
					break
				}
				continue
			}
		}
		if err := ft.client.Rename(normalizedIDs[i], newNames[i]); err != nil {
			result.Items[i].Status = "failed"
			result.Items[i].Error = err.Error()
			result.Failed++
			if !args.ContinueOnError {
				for j := i + 1; j < len(objects); j++ {
					result.Items[j].Status = "not_run"
				}
				result.Remaining = len(objects) - i - 1
				break
			}
			continue
		}
		result.Items[i].Status = "succeeded"
		result.Succeeded++
	}
	return mutationBatchCallResult(result)
}
