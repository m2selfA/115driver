package tools

import (
	"context"
	"fmt"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPInspectPaths = 128

// InspectPathsArgs defines a bounded read-only path inspection batch.
type InspectPathsArgs struct {
	Paths []string `json:"paths" jsonschema:"remote 115 paths to resolve and inspect; duplicate logical paths are rejected"`
}

// InspectPathItemResult combines path resolution with safe object metadata.
// Resolved remains true when the path was resolved but the follow-up metadata
// read failed, so agents can distinguish name-resolution failures from metadata
// failures without parsing error strings.
type InspectPathItemResult struct {
	Index            int                `json:"index" jsonschema:"zero-based input path index"`
	Path             string             `json:"path" jsonschema:"requested remote path"`
	Resolved         bool               `json:"resolved" jsonschema:"whether the path resolved to a stable object ID"`
	FileID           string             `json:"file_id,omitempty" jsonschema:"resolved 115 object ID"`
	IsDirectory      bool               `json:"is_directory" jsonschema:"whether the resolved object is a directory"`
	MetadataComplete bool               `json:"metadata_complete" jsonschema:"whether metadata came from the remote object metadata endpoint; root metadata is intentionally synthetic and partial"`
	Success          bool               `json:"success" jsonschema:"whether the path was resolved and a safe metadata view was produced"`
	Error            string             `json:"error,omitempty" jsonschema:"item error when resolution or metadata lookup failed"`
	Entry            *MCPDirectoryEntry `json:"entry,omitempty" jsonschema:"safe metadata view without thumbnail or signed URLs"`
}

// InspectPathsResult summarizes one bounded read-only inspection batch.
type InspectPathsResult struct {
	Requested int                     `json:"requested" jsonschema:"number of requested paths"`
	Succeeded int                     `json:"succeeded" jsonschema:"number of successfully inspected paths"`
	Failed    int                     `json:"failed" jsonschema:"number of paths that could not be fully inspected"`
	Items     []InspectPathItemResult `json:"items" jsonschema:"per-path results in input order"`
}

func normalizeMCPInspectPaths(paths []string) ([]string, error) {
	if len(paths) > maxMCPInspectPaths {
		return nil, fmt.Errorf("inspect path batch has %d items; maximum is %d", len(paths), maxMCPInspectPaths)
	}
	return normalizeMCPResolvePaths(paths)
}

func inspectPathsCallResult(response InspectPathsResult) (*mcp.CallToolResult, InspectPathsResult, error) {
	return mcpTypedJSONResult("inspect_paths", response, response, response.Failed > 0)
}

func (dt *DirTools) inspectPaths(ctx context.Context, req *mcp.CallToolRequest, args InspectPathsArgs) (*mcp.CallToolResult, InspectPathsResult, error) {
	paths, err := normalizeMCPInspectPaths(args.Paths)
	if err != nil {
		return toolError(fmt.Sprintf("inspect_paths preflight failed: %v", err)), InspectPathsResult{}, nil
	}
	if dt.client == nil {
		return toolError("115 client is unavailable"), InspectPathsResult{}, nil
	}

	snapshotClient := newMCPResolveSnapshotClient(dt.client)
	resolver := remoteresolver.New(snapshotClient)
	response := InspectPathsResult{Requested: len(paths), Items: make([]InspectPathItemResult, len(paths))}
	for i, remotePath := range paths {
		entry := InspectPathItemResult{Index: i, Path: remotePath}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}

		fileID, isDirectory, err := resolver.ResolvePath(remotePath)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Resolved = true
		entry.FileID = fileID
		entry.IsDirectory = isDirectory

		// Root has no ordinary parent object. Avoid depending on undocumented
		// get_info behavior for file_id=0 while still giving agents a stable,
		// explicit root representation.
		if fileID == remoteresolver.RootID {
			root := MCPDirectoryEntry{FileID: remoteresolver.RootID, Name: "/", IsDirectory: true}
			entry.Entry = &root
			entry.Success = true
			response.Succeeded++
			response.Items[i] = entry
			continue
		}

		file, err := dt.client.GetFile(fileID)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if file == nil {
			entry.Error = fmt.Sprintf("metadata for %q returned no object: %v", remotePath, driver.ErrUnexpected)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if file.IsDirectory != isDirectory {
			entry.Error = fmt.Sprintf("resolved object type changed while inspecting %q: %v", remotePath, driver.ErrUnexpected)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if file.FileID != fileID {
			entry.Error = fmt.Sprintf("resolved object id changed while inspecting %q: %v", remotePath, driver.ErrUnexpected)
			response.Failed++
			response.Items[i] = entry
			continue
		}

		metadata := mcpDirectoryEntryFromFile(*file)
		entry.Entry = &metadata
		entry.MetadataComplete = true
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return inspectPathsCallResult(response)
}
