// Package dingtalk implements the DingTalk (钉钉) document data source
// connector for WeKnora.
//
// It syncs online documents (category=ALIDOC) from DingTalk knowledge bases
// (钉钉知识库 / Wiki) into WeKnora knowledge bases, converting DingTalk's
// block-element tree to Markdown.
//
// DingTalk API docs (open.dingtalk.com/document):
//   - Auth:        POST https://api.dingtalk.com/v1.0/oauth2/accessToken (appKey/appSecret)
//   - Workspaces:  GET  /v2.0/wiki/workspaces
//   - Nodes:       GET  /v2.0/wiki/nodes
//   - Node detail: GET  /v2.0/wiki/nodes/{nodeId} (or batch)
//   - Doc content: GET  /v1.0/doc/suites/documents/{docKey}/blocks
//   - Mobile->uid: POST oapi.dingtalk.com/topapi/v2/user/getbymobile
//   - uid->unionId:POST oapi.dingtalk.com/topapi/v2/user/get
//
// Known limitations (v1):
//   - Only online documents (ALIDOC) are synced; uploaded files (docx/pdf/...)
//     inside a knowledge base are skipped. Deferred because the download
//     permission is documented inconsistently and it overlaps the Drive surface.
//   - Only returns first-level blocks; nested block shape is defensively
//     handled (see blocks.go).
//   - Incremental sync polls modifiedTime; DingTalk's file-change event
//     subscription is not used (reserved for a later version).
package dingtalk

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// DefaultAPIBaseURL is the DingTalk open-platform API host (new-style
	// /v1.0 and /v2.0 endpoints).
	DefaultAPIBaseURL = "https://api.dingtalk.com"
	// DefaultOAPIBaseURL is the legacy OpenAPI host used by the contact
	// endpoints (getbymobile / user get) that resolve the operator unionId.
	DefaultOAPIBaseURL = "https://oapi.dingtalk.com"

	// CategoryALIDOC marks a DingTalk online document node. Any other
	// category is an uploaded file/binary and is skipped in v1.
	CategoryALIDOC = "ALIDOC"
)

// Config holds DingTalk-specific connector configuration.
type Config struct {
	// AppKey is the Client ID of an internal enterprise application.
	AppKey string `json:"app_key"`
	// AppSecret is the Client Secret of the same application.
	AppSecret string `json:"app_secret"`
	// OperatorMobile is the phone number of the employee whose DingTalk
	// visibility scope the sync runs under. It is resolved to a unionId
	// internally so users never have to look the unionId up by hand.
	OperatorMobile string `json:"operator_mobile"`
	// APIBaseURL overrides the open-platform host (default DefaultAPIBaseURL).
	// Intended for proxies/testing; not exposed in the UI.
	APIBaseURL string `json:"base_url,omitempty"`
	// OAPIBaseURL overrides the legacy OpenAPI host (default DefaultOAPIBaseURL).
	OAPIBaseURL string `json:"oapi_base_url,omitempty"`
}

// GetAPIBaseURL returns the normalized open-platform base URL.
func (c *Config) GetAPIBaseURL() string {
	return normalizeBaseURL(c.APIBaseURL, DefaultAPIBaseURL)
}

// GetOAPIBaseURL returns the normalized legacy OpenAPI base URL.
func (c *Config) GetOAPIBaseURL() string {
	return normalizeBaseURL(c.OAPIBaseURL, DefaultOAPIBaseURL)
}

func normalizeBaseURL(raw, fallback string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return fallback
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}

// normalizeMobile strips formatting so "+86 138-0000-0000" and "13800000000"
// resolve to the same query value.
func normalizeMobile(raw string) string {
	m := strings.TrimSpace(raw)
	m = strings.ReplaceAll(m, " ", "")
	m = strings.ReplaceAll(m, "-", "")
	m = strings.TrimPrefix(m, "+86")
	m = strings.TrimPrefix(m, "86")
	return m
}

// parseConfig extracts and validates DingTalk configuration from the generic
// data source config. JSON roundtrip is used (matching the other connectors)
// so optional fields pick up their zero-value defaults.
func parseConfig(config *types.DataSourceConfig) (*Config, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	raw, err := json.Marshal(config.Credentials)
	if err != nil {
		return nil, fmt.Errorf("marshal credentials: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse dingtalk credentials: %w", err)
	}
	cfg.AppKey = strings.TrimSpace(cfg.AppKey)
	cfg.AppSecret = strings.TrimSpace(cfg.AppSecret)
	cfg.OperatorMobile = normalizeMobile(cfg.OperatorMobile)
	if cfg.AppKey == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("%w: app_key and app_secret are required", datasource.ErrInvalidCredentials)
	}
	if cfg.OperatorMobile == "" {
		return nil, fmt.Errorf("%w: operator_mobile is required", datasource.ErrInvalidCredentials)
	}
	if err := datasource.ValidateConnectorBaseURL(cfg.GetAPIBaseURL()); err != nil {
		return nil, err
	}
	if err := datasource.ValidateConnectorBaseURL(cfg.GetOAPIBaseURL()); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// --- Auth responses ---

// accessTokenResponse is the new-style /v1.0/oauth2/accessToken response.
type accessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireIn    int64  `json:"expireIn"`
}

// oapiTokenResponse is the legacy /gettoken response.
type oapiTokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// --- Contact responses (legacy oapi envelope) ---

type oapiGetUserByMobileResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Result  struct {
		UserID string `json:"userid"`
	} `json:"result"`
}

type oapiGetUserDetailResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	Result  struct {
		UnionID string `json:"unionid"`
		UserID  string `json:"userid"`
	} `json:"result"`
}

// --- Knowledge base (Wiki) responses ---

// workspace is one DingTalk knowledge base.
type workspace struct {
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
	RootNodeID  string `json:"rootNodeId"`
	URL         string `json:"url"`
}

type workspaceListResponse struct {
	Workspaces []workspace `json:"workspaces"`
	NextToken  string      `json:"nextToken"`
}

// wikiNode is a directory or document entry in a knowledge base tree.
type wikiNode struct {
	NodeID       string `json:"nodeId"`
	WorkspaceID  string `json:"workspaceId"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	Type         string `json:"type"`
	Category     string `json:"category"`
	Extension    string `json:"extension"`
	URL          string `json:"url"`
	CreatorID    string `json:"creatorId"`
	ModifierID   string `json:"modifierId"`
	CreateTime   string `json:"createTime"`
	ModifiedTime string `json:"modifiedTime"`
	HasChildren  bool   `json:"hasChildren"`
}

type nodeListResponse struct {
	Nodes     []wikiNode `json:"nodes"`
	NextToken string     `json:"nextToken"`
}

// --- Document content (block elements) ---

// blocksResponse is the /v1.0/doc/suites/documents/{docKey}/blocks envelope.
// The inner `result` shape is not fully documented, so it is kept raw and
// decoded defensively by extractBlocks.
type blocksResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
}

// flexInt tolerates numeric fields that the blocks endpoint delivers either
// as a JSON number or as a string carrying the number, e.g. the documented
// `heading.level: 2` arrives as "heading-2" from the live API. A plain
// integer decodes exactly like an int (including negatives); any other
// payload uses its first run of digits ("heading-2" → 2) and 0 when there
// is none, so one odd field cannot fail the whole document decode.
type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if n, err := strconv.Atoi(s); err == nil {
		*f = flexInt(n)
		return nil
	}
	s = strings.Trim(s, `"`)
	start := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		*f = 0
		return nil
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[start:end])
	if err != nil {
		return fmt.Errorf("parse number %s: %w", data, err)
	}
	*f = flexInt(n)
	return nil
}

// blockElement is one node of a DingTalk online document. The wire format is
// a discriminator (`blockType`) plus a same-named key carrying the properties,
// e.g. {"blockType":"paragraph","paragraph":{"text":"foo"}}.
type blockElement struct {
	BlockType string `json:"blockType"`
	ID        string `json:"id,omitempty"`
	Index     int    `json:"index,omitempty"`

	Paragraph     *paragraphProps `json:"paragraph,omitempty"`
	Heading       *headingProps   `json:"heading,omitempty"`
	Blockquote    *quoteProps     `json:"blockquote,omitempty"`
	Callout       *calloutProps   `json:"callout,omitempty"`
	Columns       *columnsProps   `json:"columns,omitempty"`
	OrderedList   *listProps      `json:"orderedList,omitempty"`
	UnorderedList *listProps      `json:"unorderedList,omitempty"`
	Table         *tableProps     `json:"table,omitempty"`
	TableRow      *tableRowProps  `json:"tableRow,omitempty"`
	TableCell     *tableCellProps `json:"tableCell,omitempty"`
}

// inlineElement is one node of a text-bearing block's children. Like block
// elements it uses a discriminator-plus-same-named-key shape; the discriminator
// (`inlineType`) is optional on the wire, so the renderer infers the kind from
// whichever property pointer is set when it is absent.
type inlineElement struct {
	InlineType string        `json:"inlineType,omitempty"`
	Text       *textProps    `json:"text,omitempty"`
	Sticker    *stickerProps `json:"sticker,omitempty"`
	Image      *imageProps   `json:"image,omitempty"`
	Link       *linkProps    `json:"link,omitempty"`
}

type indentProps struct {
	Left flexInt `json:"left"`
}

type paragraphProps struct {
	Text     string          `json:"text"`
	Indent   *indentProps    `json:"indent,omitempty"`
	Folded   bool            `json:"folded,omitempty"`
	Children []inlineElement `json:"children,omitempty"`
}

type headingProps struct {
	Text     string          `json:"text"`
	Level    flexInt         `json:"level"`
	Children []inlineElement `json:"children,omitempty"`
}

type quoteProps struct {
	Text     string          `json:"text"`
	Indent   *indentProps    `json:"indent,omitempty"`
	Children []inlineElement `json:"children,omitempty"`
}

type calloutProps struct {
	Sticker  string         `json:"sticker,omitempty"`
	ShowStk  bool           `json:"showstk,omitempty"`
	Color    string         `json:"color,omitempty"`
	Border   string         `json:"border,omitempty"`
	BgColor  string         `json:"bgcolor,omitempty"`
	Children []blockElement `json:"children,omitempty"`
}

type columnsProps struct {
	Size     int            `json:"size,omitempty"`
	NoFill   bool           `json:"noFill,omitempty"`
	Children []blockElement `json:"children,omitempty"`
}

type listObjectProps struct {
	ListID        string  `json:"listId,omitempty"`
	Level         flexInt `json:"level"`
	ListStyleType string  `json:"listStyleType,omitempty"`
}

type listProps struct {
	List     *listObjectProps `json:"list,omitempty"`
	Text     string           `json:"text"`
	Indent   *indentProps     `json:"indent,omitempty"`
	Children []inlineElement  `json:"children,omitempty"`
}

type tableProps struct {
	RowSize  flexInt        `json:"rolSize,omitempty"`
	ColSize  flexInt        `json:"colSize,omitempty"`
	Cells    [][]string     `json:"cells,omitempty"`
	Children []blockElement `json:"children,omitempty"`
}

type tableRowProps struct {
	// Cells holds this row's cell texts; a row is a flat list of cells.
	Cells    []string       `json:"cells,omitempty"`
	Children []blockElement `json:"children,omitempty"`
}

type tableCellProps struct {
	Text     string          `json:"text"`
	Children []inlineElement `json:"children,omitempty"`
	Blocks   []blockElement  `json:"blocks,omitempty"`
}

type textProps struct {
	Text      string  `json:"text"`
	Size      flexInt `json:"sz,omitempty"`
	Color     string  `json:"color,omitempty"`
	Highlight string  `json:"highlight,omitempty"`
	Bold      bool    `json:"bold,omitempty"`
	Italic    bool    `json:"italic,omitempty"`
	Strike    bool    `json:"stike,omitempty"` // wire field is "stike" per DingTalk docs
	Underline bool    `json:"underline,omitempty"`
	Fonts     string  `json:"fonts,omitempty"`
}

type stickerProps struct {
	Code string `json:"code"`
}

type imageProps struct {
	Src string `json:"src"`
}

type linkProps struct {
	Href     string          `json:"href"`
	Children []inlineElement `json:"children,omitempty"`
}

// --- Incremental sync cursor ---

// ddCursor is the connector-specific cursor persisted inside
// types.SyncCursor.ConnectorCursor. NodeTimes maps workspaceID -> nodeID ->
// modifiedTime so an incremental run can skip unchanged nodes and detect
// deletions (a node present last run but absent now). DocHashes maps
// dentryUuid -> SHA-256 of the rendered Markdown for individually-selected
// documents (resource IDs of the form "doc:<uuid>"), which have no node
// metadata to compare; a hash absent from the new cursor means the document
// is gone (deleted in DingTalk or deselected) and is reported as a deletion.
type ddCursor struct {
	LastSyncTime time.Time                    `json:"last_sync_time"`
	NodeTimes    map[string]map[string]string `json:"node_times"`
	DocHashes    map[string]string            `json:"doc_hashes"`
}

func newCursor() *ddCursor {
	return &ddCursor{
		NodeTimes: make(map[string]map[string]string),
		DocHashes: make(map[string]string),
	}
}

// encodeCursor wraps a ddCursor into the generic SyncCursor envelope.
func encodeCursor(c *ddCursor) *types.SyncCursor {
	m := make(map[string]interface{})
	if b, err := json.Marshal(c); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return &types.SyncCursor{
		LastSyncTime:    c.LastSyncTime,
		ConnectorCursor: m,
	}
}

// decodeCursor reads a previously persisted cursor. A nil cursor yields an
// empty ddCursor (the full-sync baseline).
func decodeCursor(cursor *types.SyncCursor) *ddCursor {
	out := newCursor()
	if cursor == nil || cursor.ConnectorCursor == nil {
		return out
	}
	b, err := json.Marshal(cursor.ConnectorCursor)
	if err != nil {
		return out
	}
	var decoded ddCursor
	if err := json.Unmarshal(b, &decoded); err != nil {
		return out
	}
	if decoded.NodeTimes != nil {
		out.NodeTimes = decoded.NodeTimes
	}
	if decoded.DocHashes != nil {
		out.DocHashes = decoded.DocHashes
	}
	out.LastSyncTime = decoded.LastSyncTime
	return out
}

// buildDocURL returns the browser URL for a knowledge base online document.
// The docKey segment is the dentryUuid taken from the node's URL/nodeId.
func buildDocURL(docKey string) string {
	if docKey == "" {
		return ""
	}
	return "https://alidocs.dingtalk.com/i/nodes/" + url.PathEscape(docKey)
}

// dingTalkTimeLayouts covers the shapes DingTalk returns for createTime /
// modifiedTime. The common form omits seconds ("2023-05-15T11:29Z"), which
// time.RFC3339 rejects, so it is tried first.
var dingTalkTimeLayouts = []string{
	"2006-01-02T15:04Z07:00",
	time.RFC3339,
	"2006-01-02T15:04:05.999Z07:00",
}

func parseDingTalkTime(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range dingTalkTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
