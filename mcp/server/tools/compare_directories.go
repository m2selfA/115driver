package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CompareDirectoriesArgs defines a bounded read-only comparison between two
// remote directory trees.
type CompareDirectoriesArgs struct {
	LeftPath         string `json:"left_path" jsonschema:"left remote 115 directory path"`
	RightPath        string `json:"right_path" jsonschema:"right remote 115 directory path"`
	MaxDepth         int    `json:"max_depth,omitempty" jsonschema:"maximum descendant depth; 0 means unlimited"`
	MaxNodes         int    `json:"max_nodes,omitempty" jsonschema:"aggregate returned-entry budget across both roots; default 1000, maximum 5000"`
	IncludeUnchanged bool   `json:"include_unchanged,omitempty" jsonschema:"include unchanged matched entries in the response; unchanged_count is always returned"`
}

// MCPDirectoryCompareSide reports the traversal evidence available for one
// side of a directory comparison.
type MCPDirectoryCompareSide struct {
	Path         string `json:"path" jsonschema:"requested remote directory path"`
	NodeBudget   int    `json:"node_budget" jsonschema:"final node budget assigned to this side"`
	NodesVisited int    `json:"nodes_visited" jsonschema:"returned descendants counted against the aggregate budget"`
	Success      bool   `json:"success" jsonschema:"whether traversal produced a bounded tree snapshot"`
	Error        string `json:"error,omitempty" jsonschema:"traversal error when this side could not be read"`
	Complete     bool   `json:"complete" jsonschema:"whether the full subtree was listed without depth or node limits"`
	DepthLimited bool   `json:"depth_limited" jsonschema:"whether max_depth intentionally bounded descendant traversal"`
	NodeLimited  bool   `json:"node_limited" jsonschema:"whether max_nodes stopped this side early"`
}

// MCPDirectoryComparePair contains both objects for a matched relative path.
// File IDs, parent IDs, and pick codes remain visible as ordinary 115 metadata
// but are not used to decide metadata equality.
type MCPDirectoryComparePair struct {
	RelativePath  string             `json:"relative_path" jsonschema:"path relative to both compared roots"`
	ChangedFields []string           `json:"changed_fields,omitempty" jsonschema:"stable comparison fields that differ"`
	Left          MCPRemoteTreeEntry `json:"left" jsonschema:"left-side object"`
	Right         MCPRemoteTreeEntry `json:"right" jsonschema:"right-side object"`
}

// CompareDirectoriesResult reports only absence claims supported by complete
// evidence on the opposite side. When node truncation or a traversal failure
// makes absence unknowable, unmatched entries are placed in unverified_*.
type CompareDirectoriesResult struct {
	Left                      MCPDirectoryCompareSide   `json:"left" jsonschema:"left traversal evidence"`
	Right                     MCPDirectoryCompareSide   `json:"right" jsonschema:"right traversal evidence"`
	MaxDepth                  int                       `json:"max_depth" jsonschema:"requested maximum traversal depth"`
	MaxNodes                  int                       `json:"max_nodes" jsonschema:"aggregate node budget across both roots"`
	NodesVisited              int                       `json:"nodes_visited" jsonschema:"aggregate descendants retained across both roots"`
	BudgetExhausted           bool                      `json:"budget_exhausted" jsonschema:"whether either side was node-limited"`
	Complete                  bool                      `json:"complete" jsonschema:"whether both full subtrees were read without depth or node limits"`
	AbsenceComparisonComplete bool                      `json:"absence_comparison_complete" jsonschema:"whether unmatched visible entries can be safely classified on both sides"`
	OnlyLeft                  []MCPRemoteTreeEntry      `json:"only_left,omitempty" jsonschema:"entries proven present only on the left within the compared scope"`
	OnlyRight                 []MCPRemoteTreeEntry      `json:"only_right,omitempty" jsonschema:"entries proven present only on the right within the compared scope"`
	TypeChanged               []MCPDirectoryComparePair `json:"type_changed,omitempty" jsonschema:"matched relative paths whose file/directory type differs"`
	MetadataChanged           []MCPDirectoryComparePair `json:"metadata_changed,omitempty" jsonschema:"matched same-type paths whose stable comparison metadata differs"`
	Unchanged                 []MCPDirectoryComparePair `json:"unchanged,omitempty" jsonschema:"matched paths with equal stable comparison metadata when requested"`
	UnverifiedLeft            []MCPRemoteTreeEntry      `json:"unverified_left,omitempty" jsonschema:"left entries whose absence on the right cannot be proven because right evidence is node-truncated or failed"`
	UnverifiedRight           []MCPRemoteTreeEntry      `json:"unverified_right,omitempty" jsonschema:"right entries whose absence on the left cannot be proven because left evidence is node-truncated or failed"`
	OnlyLeftCount             int                       `json:"only_left_count"`
	OnlyRightCount            int                       `json:"only_right_count"`
	TypeChangedCount          int                       `json:"type_changed_count"`
	MetadataChangedCount      int                       `json:"metadata_changed_count"`
	UnchangedCount            int                       `json:"unchanged_count"`
	UnverifiedLeftCount       int                       `json:"unverified_left_count"`
	UnverifiedRightCount      int                       `json:"unverified_right_count"`
}

func normalizeMCPCompareDirectoriesArgs(args CompareDirectoriesArgs) (string, string, int, error) {
	paths, maxNodes, err := normalizeMCPListTreeArgs(ListTreeArgs{
		Paths:    []string{args.LeftPath, args.RightPath},
		MaxDepth: args.MaxDepth,
		MaxNodes: args.MaxNodes,
	})
	if err != nil {
		return "", "", 0, err
	}
	if maxNodes < 2 {
		return "", "", 0, fmt.Errorf("max_nodes must be at least 2 for a two-sided comparison")
	}
	return paths[0], paths[1], maxNodes, nil
}

func mcpDirectoryCompareSide(item ListTreeItemResult, budget int) MCPDirectoryCompareSide {
	return MCPDirectoryCompareSide{
		Path:         item.Path,
		NodeBudget:   budget,
		NodesVisited: item.NodesVisited,
		Success:      item.Success,
		Error:        item.Error,
		Complete:     item.Complete,
		DepthLimited: item.DepthLimited,
		NodeLimited:  item.NodeLimited,
	}
}

func compareTreeEntryMetadata(left, right MCPRemoteTreeEntry) []string {
	changed := make([]string, 0, 3)
	if !left.IsDirectory && left.Size != right.Size {
		changed = append(changed, "size")
	}
	if !left.IsDirectory && left.SHA1 != right.SHA1 {
		changed = append(changed, "sha1")
	}
	if left.Star != right.Star {
		changed = append(changed, "star")
	}
	return changed
}

func compareMCPDirectorySnapshots(left, right ListTreeItemResult, leftBudget, rightBudget, maxDepth, maxNodes int, includeUnchanged bool) (CompareDirectoriesResult, error) {
	response := CompareDirectoriesResult{
		Left:            mcpDirectoryCompareSide(left, leftBudget),
		Right:           mcpDirectoryCompareSide(right, rightBudget),
		MaxDepth:        maxDepth,
		MaxNodes:        maxNodes,
		NodesVisited:    left.NodesVisited + right.NodesVisited,
		BudgetExhausted: left.NodeLimited || right.NodeLimited,
		Complete:        left.Success && right.Success && left.Complete && right.Complete,
	}
	leftAbsenceEvidence := left.Success && !left.NodeLimited
	rightAbsenceEvidence := right.Success && !right.NodeLimited
	response.AbsenceComparisonComplete = leftAbsenceEvidence && rightAbsenceEvidence

	leftByPath := make(map[string]MCPRemoteTreeEntry, len(left.Entries))
	rightByPath := make(map[string]MCPRemoteTreeEntry, len(right.Entries))
	keys := make(map[string]struct{}, len(left.Entries)+len(right.Entries))
	for _, entry := range left.Entries {
		if _, exists := leftByPath[entry.RelativePath]; exists {
			return CompareDirectoriesResult{}, fmt.Errorf("left traversal returned duplicate relative path %q", entry.RelativePath)
		}
		leftByPath[entry.RelativePath] = entry
		keys[entry.RelativePath] = struct{}{}
	}
	for _, entry := range right.Entries {
		if _, exists := rightByPath[entry.RelativePath]; exists {
			return CompareDirectoriesResult{}, fmt.Errorf("right traversal returned duplicate relative path %q", entry.RelativePath)
		}
		rightByPath[entry.RelativePath] = entry
		keys[entry.RelativePath] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	for _, relativePath := range ordered {
		leftEntry, hasLeft := leftByPath[relativePath]
		rightEntry, hasRight := rightByPath[relativePath]
		switch {
		case hasLeft && hasRight:
			pair := MCPDirectoryComparePair{RelativePath: relativePath, Left: leftEntry, Right: rightEntry}
			if leftEntry.IsDirectory != rightEntry.IsDirectory {
				pair.ChangedFields = []string{"is_directory"}
				response.TypeChanged = append(response.TypeChanged, pair)
				continue
			}
			pair.ChangedFields = compareTreeEntryMetadata(leftEntry, rightEntry)
			if len(pair.ChangedFields) > 0 {
				response.MetadataChanged = append(response.MetadataChanged, pair)
				continue
			}
			response.UnchangedCount++
			if includeUnchanged {
				response.Unchanged = append(response.Unchanged, pair)
			}
		case hasLeft:
			if rightAbsenceEvidence {
				response.OnlyLeft = append(response.OnlyLeft, leftEntry)
			} else {
				response.UnverifiedLeft = append(response.UnverifiedLeft, leftEntry)
			}
		case hasRight:
			if leftAbsenceEvidence {
				response.OnlyRight = append(response.OnlyRight, rightEntry)
			} else {
				response.UnverifiedRight = append(response.UnverifiedRight, rightEntry)
			}
		}
	}

	response.OnlyLeftCount = len(response.OnlyLeft)
	response.OnlyRightCount = len(response.OnlyRight)
	response.TypeChangedCount = len(response.TypeChanged)
	response.MetadataChangedCount = len(response.MetadataChanged)
	response.UnverifiedLeftCount = len(response.UnverifiedLeft)
	response.UnverifiedRightCount = len(response.UnverifiedRight)
	return response, nil
}

func compareMCPDirectories(ctx context.Context, client mcpListTreeClient, args CompareDirectoriesArgs) (CompareDirectoriesResult, error) {
	leftPath, rightPath, maxNodes, err := normalizeMCPCompareDirectoriesArgs(args)
	if err != nil {
		return CompareDirectoriesResult{}, err
	}
	if client == nil {
		return CompareDirectoriesResult{}, fmt.Errorf("115 client is unavailable")
	}

	// Start with a fair split so one huge tree cannot starve the other. The
	// second side then receives any nodes unused by the first. If the first side
	// was node-limited and the second completed early, one cached retry gives the
	// first side the remaining budget without repeating remote page reads.
	sharedSnapshot := newMCPListTreeSnapshotClient(client)
	leftBudget := (maxNodes + 1) / 2
	leftTree, err := listMCPRemoteTree(ctx, sharedSnapshot, ListTreeArgs{Paths: []string{leftPath}, MaxDepth: args.MaxDepth, MaxNodes: leftBudget})
	if err != nil {
		return CompareDirectoriesResult{}, err
	}
	left := leftTree.Items[0]
	// A side that stops without hitting its cap gives unused capacity back to
	// the aggregate budget. Report the retained/final budget rather than the
	// initial fair-split allowance.
	if !left.NodeLimited {
		leftBudget = left.NodesVisited
	}

	rightBudget := maxNodes - left.NodesVisited
	if rightBudget < 1 {
		rightBudget = 1
	}
	rightTree, err := listMCPRemoteTree(ctx, sharedSnapshot, ListTreeArgs{Paths: []string{rightPath}, MaxDepth: args.MaxDepth, MaxNodes: rightBudget})
	if err != nil {
		return CompareDirectoriesResult{}, err
	}
	right := rightTree.Items[0]

	if left.Success && left.NodeLimited && right.Success && !right.NodeLimited {
		expandedLeftBudget := maxNodes - right.NodesVisited
		if expandedLeftBudget > leftBudget {
			retry, retryErr := listMCPRemoteTree(ctx, sharedSnapshot, ListTreeArgs{Paths: []string{leftPath}, MaxDepth: args.MaxDepth, MaxNodes: expandedLeftBudget})
			if retryErr != nil {
				return CompareDirectoriesResult{}, retryErr
			}
			leftBudget = expandedLeftBudget
			left = retry.Items[0]
			if !left.NodeLimited {
				leftBudget = left.NodesVisited
			}
		}
	}
	if !right.NodeLimited {
		rightBudget = right.NodesVisited
	}

	return compareMCPDirectorySnapshots(left, right, leftBudget, rightBudget, args.MaxDepth, maxNodes, args.IncludeUnchanged)
}

func compareDirectoriesCallResult(response CompareDirectoriesResult) (*mcp.CallToolResult, CompareDirectoriesResult, error) {
	return mcpTypedJSONResult("compare_directories", response, response, !response.Left.Success || !response.Right.Success)
}

func (dt *DirTools) compareDirectories(ctx context.Context, req *mcp.CallToolRequest, args CompareDirectoriesArgs) (*mcp.CallToolResult, CompareDirectoriesResult, error) {
	response, err := compareMCPDirectories(ctx, dt.client, args)
	if err != nil {
		return toolError(fmt.Sprintf("compare_directories preflight failed: %v", err)), CompareDirectoriesResult{}, nil
	}
	return compareDirectoriesCallResult(response)
}
