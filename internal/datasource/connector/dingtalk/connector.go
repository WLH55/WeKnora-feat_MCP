package dingtalk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// Compile-time proof that *Connector satisfies both interfaces.
var (
	_ datasource.Connector          = (*Connector)(nil)
	_ datasource.StreamingConnector = (*Connector)(nil)
)

// resourceIDSeparator joins workspace and node inside a resource ID. Node IDs
// (dentryUuid) never contain "/", so splitting on the first occurrence is safe.
const resourceIDSeparator = "/"

// bareDocPrefix marks a resource ID that addresses one document by its
// dentryUuid instead of a knowledge base (sub)tree. Colons never occur in
// DingTalk workspace IDs, so the two resource ID families cannot collide.
const bareDocPrefix = "doc:"

// Connector implements datasource.Connector for DingTalk documents.
type Connector struct{}

// NewConnector creates a new DingTalk connector.
func NewConnector() *Connector { return &Connector{} }

// Type returns the connector type identifier.
func (c *Connector) Type() string { return types.ConnectorTypeDingTalk }

// Validate verifies credentials and connectivity: it obtains a token, resolves
// the operator mobile to a unionId, and lists one page of knowledge bases.
func (c *Connector) Validate(ctx context.Context, config *types.DataSourceConfig) error {
	cfg, err := parseConfig(config)
	if err != nil {
		return err
	}
	cli := newClient(cfg)
	operatorID, err := cli.resolveOperatorID(ctx)
	if err != nil {
		return fmt.Errorf("dingtalk connection failed: %w", err)
	}
	if _, _, err := cli.listWorkspaces(ctx, operatorID, ""); err != nil {
		return fmt.Errorf("dingtalk connection failed: %w", err)
	}
	return nil
}

// ListResources lists syncable resources.
//
//   - parentID == ""  → knowledge bases (top level).
//   - parentID != ""  → direct children of that node, enabling lazy tree
//     expansion. A parentID may be a bare workspace ID (expanded from the
//     workspace root) or a `<workspaceID>/<nodeID>` pair.
func (c *Connector) ListResources(
	ctx context.Context, config *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	cli := newClient(cfg)
	operatorID, err := cli.resolveOperatorID(ctx)
	if err != nil {
		return nil, err
	}

	if parentID == "" {
		workspaces, err := cli.listAllWorkspaces(ctx, operatorID)
		if err != nil {
			return nil, fmt.Errorf("list knowledge bases: %w", err)
		}
		out := make([]types.Resource, 0, len(workspaces))
		for _, w := range workspaces {
			out = append(out, types.Resource{
				ExternalID:  w.WorkspaceID,
				Name:        w.Name,
				Type:        "knowledge_base",
				URL:         w.URL,
				HasChildren: true,
				Metadata: map[string]interface{}{
					"workspace_id": w.WorkspaceID,
					"root_node_id": w.RootNodeID,
				},
			})
		}
		return out, nil
	}

	workspaceID, nodeID := parseResourceID(parentID)
	if nodeID == "" {
		root, err := c.rootNodeID(ctx, cli, operatorID, workspaceID)
		if err != nil {
			return nil, err
		}
		nodeID = root
	}

	nodes, err := cli.listAllNodes(ctx, operatorID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list nodes under %s: %w", nodeID, err)
	}
	out := make([]types.Resource, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, types.Resource{
			ExternalID:  makeResourceID(workspaceID, n.NodeID),
			Name:        n.Name,
			Type:        resourceTypeFor(n),
			URL:         n.URL,
			ModifiedAt:  parseDingTalkTime(n.ModifiedTime),
			ParentID:    parentID,
			HasChildren: n.HasChildren,
			Metadata: map[string]interface{}{
				"category":     n.Category,
				"workspace_id": workspaceID,
				"node_id":      n.NodeID,
			},
		})
	}
	return out, nil
}

// ResolveResourceAncestors returns no ancestors. DingTalk's node object exposes
// no parent pointer, so a selected node's path back to the root cannot be
// derived without re-walking the whole tree. Returning an empty slice only
// means the picker does not auto-expand a previously saved selection; it does
// not affect which resources are synced.
func (c *Connector) ResolveResourceAncestors(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]string, error) {
	return []string{}, nil
}

// FetchAll performs a full sync of the selected resources.
func (c *Connector) FetchAll(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]types.FetchedItem, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	var items []types.FetchedItem
	_, err = c.walk(ctx, cfg, resourceIDs, nil, false,
		func(_ context.Context, item types.FetchedItem) error {
			items = append(items, item)
			return nil
		}, nil)
	if err != nil {
		return nil, err
	}
	return items, nil
}

// FetchIncremental fetches only changed items since the given cursor.
func (c *Connector) FetchIncremental(
	ctx context.Context, config *types.DataSourceConfig, cursor *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, nil, err
	}
	prev := decodeCursor(cursor)
	var items []types.FetchedItem
	next, err := c.walk(ctx, cfg, config.ResourceIDs, prev, true,
		func(_ context.Context, item types.FetchedItem) error {
			items = append(items, item)
			return nil
		}, nil)
	if err != nil {
		return nil, nil, err
	}
	return items, encodeCursor(next), nil
}

// FetchStream walks the tree while emitting each item and checkpointing at page
// boundaries, so a large knowledge base syncs incrementally and resumes after a
// timeout.
func (c *Connector) FetchStream(
	ctx context.Context, config *types.DataSourceConfig,
	cursor *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	prev := decodeCursor(cursor)
	// A nil cursor is a full sync (streamStartCursor returns nil for the first
	// attempt of a forced full run); a non-nil cursor means incremental, or a
	// resumed full sync whose progress was checkpointed.
	skipUnchanged := cursor != nil && len(prev.NodeTimes) > 0
	next, err := c.walk(ctx, cfg, config.ResourceIDs, prev, skipUnchanged, h.Emit, h.Checkpoint)
	if err != nil {
		return nil, err
	}
	return encodeCursor(next), nil
}

// walk traverses the selected resources, emitting document items and building
// the next cursor. increment, when true, skips nodes whose modifiedTime matches
// the previous run, and reports nodes that disappeared as deletions.
func (c *Connector) walk(
	ctx context.Context,
	cfg *Config,
	resourceIDs []string,
	prev *ddCursor,
	incremental bool,
	emit func(context.Context, types.FetchedItem) error,
	checkpoint func(context.Context, *types.SyncCursor) error,
) (*ddCursor, error) {
	if len(resourceIDs) == 0 {
		return nil, fmt.Errorf("no resources selected; pick at least one knowledge base")
	}

	cli := newClient(cfg)
	operatorID, err := cli.resolveOperatorID(ctx)
	if err != nil {
		return nil, err
	}

	workspaces, err := cli.listAllWorkspaces(ctx, operatorID)
	if err != nil {
		return nil, fmt.Errorf("list knowledge bases: %w", err)
	}
	rootByWorkspace := make(map[string]string, len(workspaces))
	for _, w := range workspaces {
		rootByWorkspace[w.WorkspaceID] = w.RootNodeID
	}

	cur := newCursor()
	cur.LastSyncTime = time.Now().UTC()

	var skipped int
	for _, rid := range resourceIDs {
		if docKey := bareDocID(rid); docKey != "" {
			if err := c.syncBareDoc(ctx, cli, operatorID, docKey, prev, cur, incremental, emit); err != nil {
				return nil, err
			}
			continue
		}
		workspaceID, startNode := parseResourceID(rid)
		if startNode == "" {
			root, ok := rootByWorkspace[workspaceID]
			if !ok {
				return nil, fmt.Errorf("knowledge base %s is not visible to the configured operator", workspaceID)
			}
			startNode = root
		}
		if startNode == "" {
			return nil, fmt.Errorf("knowledge base %s has no root node", workspaceID)
		}
		if _, ok := cur.NodeTimes[workspaceID]; !ok {
			cur.NodeTimes[workspaceID] = make(map[string]string)
		}
		if err := c.walkTree(ctx, cli, operatorID, workspaceID, startNode, rid,
			prev, incremental, cur, emit, checkpoint, &skipped); err != nil {
			return nil, err
		}
	}

	// Deletions: nodes recorded last run but absent this run.
	if prev != nil {
		for workspaceID, prevNodes := range prev.NodeTimes {
			curNodes := cur.NodeTimes[workspaceID]
			for nodeID := range prevNodes {
				if _, seen := curNodes[nodeID]; seen {
					continue
				}
				if err := emit(ctx, types.FetchedItem{
					ExternalID:       nodeID,
					SourceResourceID: makeResourceID(workspaceID, ""),
					IsDeleted:        true,
					Metadata: map[string]string{
						"channel":      types.ChannelDingtalk,
						"workspace_id": workspaceID,
					},
				}); err != nil {
					return nil, err
				}
			}
		}

		// Bare documents follow the same diff rule: recorded last run but
		// absent now — deleted in DingTalk (syncBareDoc saw a 404 and left it
		// out of the new cursor) or deselected in the editor.
		for docKey := range prev.DocHashes {
			if _, seen := cur.DocHashes[docKey]; seen {
				continue
			}
			if err := emit(ctx, types.FetchedItem{
				ExternalID:       docKey,
				SourceResourceID: makeBareDocID(docKey),
				IsDeleted:        true,
				Metadata: map[string]string{
					"channel": types.ChannelDingtalk,
					"doc_key": docKey,
				},
			}); err != nil {
				return nil, err
			}
		}
	}

	if skipped > 0 {
		logger.Infof(ctx, "[DingTalk] incremental sync skipped %d unchanged node(s)", skipped)
	}
	return cur, nil
}

// walkTree depth-first traverses nodeID's subtree, paginating each level.
func (c *Connector) walkTree(
	ctx context.Context,
	cli *client,
	operatorID, workspaceID, nodeID, resourceID string,
	prev *ddCursor,
	incremental bool,
	cur *ddCursor,
	emit func(context.Context, types.FetchedItem) error,
	checkpoint func(context.Context, *types.SyncCursor) error,
	skipped *int,
) error {
	nextToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nodes, token, err := cli.listNodes(ctx, operatorID, nodeID, nextToken)
		if err != nil {
			return fmt.Errorf("list nodes under %s: %w", nodeID, err)
		}
		for _, n := range nodes {
			cur.NodeTimes[workspaceID][n.NodeID] = n.ModifiedTime

			unchanged := false
			if incremental && prev != nil {
				if prevTime, ok := prev.NodeTimes[workspaceID][n.NodeID]; ok && prevTime == n.ModifiedTime {
					unchanged = true
				}
			}

			switch {
			case n.Category != CategoryALIDOC:
				// Uploaded files (docx/pdf/...) are out of scope in v1.
				if !unchanged {
					logger.Infof(ctx, "[DingTalk] skip node %s (%q): category=%q is not an online document",
						n.NodeID, n.Name, n.Category)
				}
			case unchanged:
				*skipped++
			default:
				if err := c.emitDocument(ctx, cli, operatorID, workspaceID, resourceID, n, emit); err != nil {
					// One bad document must not fail the sync, but it must not
					// vanish either: emit a fetch-failed item so the sync log
					// counts it failed instead of showing a silent "success"
					// (the visibility gap behind 7×403 with status success).
					logger.Warnf(ctx, "[DingTalk] failed to fetch document %s (%q): %v", n.NodeID, n.Name, err)
					if emitErr := emit(ctx, types.FetchedItem{
						ExternalID:       n.NodeID,
						Title:            n.Name,
						URL:              n.URL,
						SourceResourceID: resourceID,
						FetchError:       err.Error(),
						Metadata: map[string]string{
							"channel":      types.ChannelDingtalk,
							"workspace_id": workspaceID,
							"node_id":      n.NodeID,
						},
					}); emitErr != nil {
						return emitErr
					}
				}
			}

			if n.HasChildren {
				if err := c.walkTree(ctx, cli, operatorID, workspaceID, n.NodeID, resourceID,
					prev, incremental, cur, emit, checkpoint, skipped); err != nil {
					return err
				}
			}
		}

		if token == "" || len(nodes) == 0 {
			return nil
		}
		nextToken = token
		if checkpoint != nil {
			if err := checkpoint(ctx, encodeCursor(cur)); err != nil {
				return err
			}
		}
	}
}

// emitDocument fetches an online document's blocks, renders them to Markdown,
// and emits the resulting item.
func (c *Connector) emitDocument(
	ctx context.Context,
	cli *client,
	operatorID, workspaceID, resourceID string,
	n wikiNode,
	emit func(context.Context, types.FetchedItem) error,
) error {
	docKey := docKeyOf(n)
	blocks, err := cli.queryBlocks(ctx, operatorID, docKey)
	if err != nil {
		return fmt.Errorf("query blocks: %w", err)
	}
	content, err := blocksToMarkdown(blocks)
	if err != nil {
		return fmt.Errorf("render blocks: %w", err)
	}

	title := strings.TrimSpace(n.Name)
	if title == "" {
		title = n.NodeID
	}
	url := strings.TrimSpace(n.URL)
	if url == "" || !strings.Contains(url, "://") {
		url = buildDocURL(docKey)
	}
	return emit(ctx, types.FetchedItem{
		ExternalID:       n.NodeID,
		Title:            title,
		Content:          content,
		ContentType:      "text/markdown",
		FileName:         sanitizeFileName(title) + ".md",
		URL:              url,
		UpdatedAt:        parseDingTalkTime(n.ModifiedTime),
		SourceResourceID: resourceID,
		Metadata: map[string]string{
			"channel":      types.ChannelDingtalk,
			"workspace_id": workspaceID,
			"node_id":      n.NodeID,
			"doc_key":      docKey,
			"category":     n.Category,
		},
	})
}

// rootNodeID resolves a workspace's root node ID.
func (c *Connector) rootNodeID(
	ctx context.Context, cli *client, operatorID, workspaceID string,
) (string, error) {
	workspaces, err := cli.listAllWorkspaces(ctx, operatorID)
	if err != nil {
		return "", err
	}
	for _, w := range workspaces {
		if w.WorkspaceID == workspaceID {
			return w.RootNodeID, nil
		}
	}
	return "", fmt.Errorf("knowledge base %s is not visible to the configured operator", workspaceID)
}

// syncBareDoc fetches a single document addressed by dentryUuid, bypassing
// knowledge base traversal entirely. The rendered Markdown is hashed into the
// cursor for incremental skip. A 404 leaves the doc out of the new cursor so
// the walk-level deletion diff reports it; any other failure emits a
// FetchError item (existing copy kept, counted failed, retried next run).
func (c *Connector) syncBareDoc(
	ctx context.Context,
	cli *client,
	operatorID, docKey string,
	prev, cur *ddCursor,
	incremental bool,
	emit func(context.Context, types.FetchedItem) error,
) error {
	blocks, err := cli.queryBlocks(ctx, operatorID, docKey)
	if err != nil {
		return handleBareDocFailure(ctx, docKey, err, prev, cur, emit)
	}
	content, err := blocksToMarkdown(blocks)
	if err != nil {
		return handleBareDocFailure(ctx, docKey, fmt.Errorf("render blocks: %w", err), prev, cur, emit)
	}

	sum := sha256Hex(content)
	if incremental && prev != nil && prev.DocHashes[docKey] == sum {
		cur.DocHashes[docKey] = sum
		logger.Infof(ctx, "[DingTalk] incremental sync skipped unchanged document %s", docKey)
		return nil
	}
	cur.DocHashes[docKey] = sum

	title := docTitleFromBlocks(blocks, docKey)
	return emit(ctx, types.FetchedItem{
		ExternalID:       docKey,
		Title:            title,
		Content:          content,
		ContentType:      "text/markdown",
		FileName:         sanitizeFileName(title) + ".md",
		URL:              buildDocURL(docKey),
		SourceResourceID: makeBareDocID(docKey),
		Metadata: map[string]string{
			"channel":  types.ChannelDingtalk,
			"doc_key":  docKey,
			"bare_doc": "true",
		},
	})
}

// handleBareDocFailure classifies a bare-doc fetch/render failure. A 404 means
// the document is gone: it is deliberately left out of the new cursor so the
// deletion diff emits IsDeleted. Anything else (403, 5xx exhausted, decode
// error) keeps the document alive — the previous hash is carried forward so
// the deletion diff stays quiet, and a FetchError item is emitted so the sync
// log counts it as failed and the next run retries.
func handleBareDocFailure(
	ctx context.Context,
	docKey string,
	err error,
	prev, cur *ddCursor,
	emit func(context.Context, types.FetchedItem) error,
) error {
	var statusErr *apiStatusError
	if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
		logger.Infof(ctx, "[DingTalk] document %s is gone (404); will be reported as deleted", docKey)
		return nil
	}
	if prev != nil {
		if h, ok := prev.DocHashes[docKey]; ok {
			cur.DocHashes[docKey] = h
		}
	}
	logger.Warnf(ctx, "[DingTalk] failed to fetch document %s: %v", docKey, err)
	return emit(ctx, types.FetchedItem{
		ExternalID: docKey,
		FetchError: err.Error(),
		URL:        buildDocURL(docKey),
		Metadata: map[string]string{
			"channel":  types.ChannelDingtalk,
			"doc_key":  docKey,
			"bare_doc": "true",
		},
	})
}

// docTitleFromBlocks picks a title for a bare document: the blocks response
// carries no title, so the first heading is the best human-readable name.
// Without one, the doc key prefix stands in so the item is still identifiable.
func docTitleFromBlocks(blocks []blockElement, docKey string) string {
	for _, b := range blocks {
		if b.Heading == nil {
			continue
		}
		if t := strings.TrimSpace(b.Heading.Text); t != "" {
			return t
		}
	}
	if len(docKey) > 8 {
		docKey = docKey[:8]
	}
	return "钉钉文档 " + docKey
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- helpers ---

func makeResourceID(workspaceID, nodeID string) string {
	if nodeID == "" {
		return workspaceID
	}
	return workspaceID + resourceIDSeparator + nodeID
}

func parseResourceID(id string) (workspaceID, nodeID string) {
	if i := strings.Index(id, resourceIDSeparator); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// makeBareDocID builds the resource ID for an individually-selected document.
func makeBareDocID(docKey string) string { return bareDocPrefix + docKey }

// bareDocID returns the dentryUuid of a bare-doc resource ID, or "" when the
// ID addresses a knowledge base (sub)tree instead.
func bareDocID(resourceID string) string {
	if docKey, ok := strings.CutPrefix(resourceID, bareDocPrefix); ok && docKey != "" {
		return docKey
	}
	return ""
}

func resourceTypeFor(n wikiNode) string {
	if n.Category == CategoryALIDOC {
		return "document"
	}
	if n.HasChildren {
		return "folder"
	}
	return "file"
}

// docKeyOf returns the document key used by the blocks endpoint. It prefers the
// dentryUuid embedded in the node URL and falls back to nodeId, which the API
// documents as equivalent (dentryUuid).
func docKeyOf(n wikiNode) string {
	if uuid := dentryUUIDFromURL(n.URL); uuid != "" {
		return uuid
	}
	return n.NodeID
}

func dentryUUIDFromURL(raw string) string {
	const marker = "/nodes/"
	i := strings.Index(raw, marker)
	if i < 0 {
		return ""
	}
	rest := raw[i+len(marker):]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// sanitizeFileName makes a document title safe to use as a file name.
func sanitizeFileName(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return "document"
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	s = replacer.Replace(s)
	if utf8.RuneCountInString(s) > 120 {
		runes := []rune(s)
		s = string(runes[:120])
	}
	return s
}
