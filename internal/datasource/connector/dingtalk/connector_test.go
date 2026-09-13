package dingtalk

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestParseConfig_RequiresCredentials(t *testing.T) {
	cases := []struct {
		name string
		cred map[string]interface{}
	}{
		{"missing everything", map[string]interface{}{}},
		{"missing secret", map[string]interface{}{"app_key": "k", "operator_mobile": "13800000000"}},
		{"missing mobile", map[string]interface{}{"app_key": "k", "app_secret": "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig(&types.DataSourceConfig{Credentials: tc.cred})
			if err == nil {
				t.Fatal("expected error for incomplete credentials")
			}
		})
	}
}

func TestParseConfig_OKAndNormalizesMobile(t *testing.T) {
	cfg, err := parseConfig(&types.DataSourceConfig{Credentials: map[string]interface{}{
		"app_key":         "  dingabc  ",
		"app_secret":      "secret",
		"operator_mobile": "+86 138-0000-0000",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AppKey != "dingabc" {
		t.Fatalf("app_key not trimmed: %q", cfg.AppKey)
	}
	if cfg.OperatorMobile != "13800000000" {
		t.Fatalf("mobile not normalized: %q", cfg.OperatorMobile)
	}
	if cfg.GetAPIBaseURL() != DefaultAPIBaseURL {
		t.Fatalf("default api base URL wrong: %q", cfg.GetAPIBaseURL())
	}
	if cfg.GetOAPIBaseURL() != DefaultOAPIBaseURL {
		t.Fatalf("default oapi base URL wrong: %q", cfg.GetOAPIBaseURL())
	}
}

func TestParseConfig_RejectsHugePageSettings(t *testing.T) {
	// Guard: base URL override must pass SSRF validation. A loopback host is
	// rejected by ValidateConnectorBaseURL, which parseConfig must surface.
	_, err := parseConfig(&types.DataSourceConfig{Credentials: map[string]interface{}{
		"app_key":         "k",
		"app_secret":      "s",
		"operator_mobile": "13800000000",
		"base_url":        "http://127.0.0.1:8080",
	}})
	if err == nil {
		t.Fatal("expected SSRF validation to reject loopback base_url")
	}
}

func TestNormalizeMobile(t *testing.T) {
	for in, want := range map[string]string{
		"13800000000":       "13800000000",
		"+86 138-0000-0000": "13800000000",
		"86 13800000000":    "13800000000",
		" 13800000000 ":     "13800000000",
	} {
		if got := normalizeMobile(in); got != want {
			t.Fatalf("normalizeMobile(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCursorEncodeDecodeRoundTrip(t *testing.T) {
	c := newCursor()
	c.LastSyncTime = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	c.NodeTimes["ws-1"] = map[string]string{"n1": "2026-09-10T10:00Z", "n2": "2026-09-09T08:00Z"}

	encoded := encodeCursor(c)
	if encoded.ConnectorCursor == nil {
		t.Fatal("encoded cursor has no ConnectorCursor payload")
	}
	decoded := decodeCursor(encoded)
	if !decoded.LastSyncTime.Equal(c.LastSyncTime) {
		t.Fatalf("last sync time lost: got %v want %v", decoded.LastSyncTime, c.LastSyncTime)
	}
	if got := decoded.NodeTimes["ws-1"]["n1"]; got != "2026-09-10T10:00Z" {
		t.Fatalf("node time lost: %q", got)
	}
	if got := decoded.NodeTimes["ws-1"]["n2"]; got != "2026-09-09T08:00Z" {
		t.Fatalf("node time lost: %q", got)
	}
}

func TestDecodeCursor_NilIsEmptyBaseline(t *testing.T) {
	d := decodeCursor(nil)
	if d == nil || len(d.NodeTimes) != 0 {
		t.Fatalf("nil cursor should decode to empty baseline, got %+v", d)
	}
	if d.LastSyncTime.IsZero() != true {
		t.Fatal("empty baseline should have zero LastSyncTime")
	}
}

func TestDecodeCursor_MalformedPayloadIsEmptyBaseline(t *testing.T) {
	d := decodeCursor(&types.SyncCursor{ConnectorCursor: map[string]interface{}{"node_times": "not-a-map"}})
	if d == nil || len(d.NodeTimes) != 0 {
		t.Fatalf("malformed cursor should fall back to empty baseline, got %+v", d)
	}
}

func TestResourceIDRoundTrip(t *testing.T) {
	id := makeResourceID("ws-1", "node-9")
	if id != "ws-1/node-9" {
		t.Fatalf("makeResourceID=%q", id)
	}
	ws, node := parseResourceID(id)
	if ws != "ws-1" || node != "node-9" {
		t.Fatalf("parseResourceID=(%q,%q)", ws, node)
	}
	// A bare workspace id yields an empty node (root expansion).
	ws2, node2 := parseResourceID("ws-2")
	if ws2 != "ws-2" || node2 != "" {
		t.Fatalf("bare parse=(%q,%q)", ws2, node2)
	}
}

func TestDentryUUIDFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://alidocs.dingtalk.com/i/nodes/Zxxxxa-id":     "Zxxxxa-id",
		"https://alidocs.dingtalk.com/i/nodes/abc123?from=x": "abc123",
		"https://alidocs.dingtalk.com/i/nodes/abc123#frag":   "abc123",
		"https://example.com/no-marker":                      "",
		"":                                                   "",
	} {
		if got := dentryUUIDFromURL(in); got != want {
			t.Fatalf("dentryUUIDFromURL(%q)=%q want %q", in, got, want)
		}
	}
}

func TestDocKeyOf_PrefersURLThenNodeID(t *testing.T) {
	if got := docKeyOf(wikiNode{NodeID: "n1", URL: "https://alidocs.dingtalk.com/i/nodes/uuid-1"}); got != "uuid-1" {
		t.Fatalf("expected uuid from URL, got %q", got)
	}
	if got := docKeyOf(wikiNode{NodeID: "n1", URL: "https://example.com/other"}); got != "n1" {
		t.Fatalf("expected nodeId fallback, got %q", got)
	}
}

func TestParseDingTalkTime(t *testing.T) {
	// The documented shape omits seconds.
	got := parseDingTalkTime("2023-05-15T11:29Z")
	if got.IsZero() {
		t.Fatal("failed to parse minute-precision timestamp")
	}
	if got.UTC().Format(time.RFC3339) != "2023-05-15T11:29:00Z" {
		t.Fatalf("unexpected parsed time: %v", got.UTC().Format(time.RFC3339))
	}
	if !parseDingTalkTime("").IsZero() {
		t.Fatal("empty input should be zero time")
	}
	if !parseDingTalkTime("garbage").IsZero() {
		t.Fatal("invalid input should be zero time")
	}
}

func TestSanitizeFileName(t *testing.T) {
	if got := sanitizeFileName("a/b:c*d?e\"f<g>h|i"); got != "a_b_c_d_e_f_g_h_i" {
		t.Fatalf("sanitizeFileName=%q", got)
	}
	if got := sanitizeFileName("   "); got != "document" {
		t.Fatalf("blank name should fall back to document, got %q", got)
	}
}

func TestExtractBlocks_AcceptsKnownShapes(t *testing.T) {
	shapes := []string{
		`[{"blockType":"paragraph","paragraph":{"text":"a"}}]`,
		`{"blocks":[{"blockType":"paragraph","paragraph":{"text":"a"}}]}`,
		`{"data":[{"blockType":"paragraph","paragraph":{"text":"a"}}]}`,
		`{"data":{"blocks":[{"blockType":"paragraph","paragraph":{"text":"a"}}]}}`,
	}
	for _, raw := range shapes {
		got, err := extractBlocks(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("shape %s: unexpected error %v", raw, err)
		}
		if len(got) != 1 || got[0].Paragraph == nil || got[0].Paragraph.Text != "a" {
			t.Fatalf("shape %s: decoded %+v", raw, got)
		}
	}
}

func TestExtractBlocks_EmptyAndUnrecognized(t *testing.T) {
	if got, err := extractBlocks(json.RawMessage(`null`)); err != nil || got != nil {
		t.Fatalf("null should yield nil,nil got %v,%v", got, err)
	}
	if _, err := extractBlocks(json.RawMessage(`"a string"`)); err == nil {
		t.Fatal("unrecognized shape should error")
	}
}

// The live blocks endpoint returns heading.level as a string ("heading-2")
// although the documentation shows a number. One such field used to fail the
// whole-array decode ("unrecognized result shape"), dropping the document.
func TestExtractBlocks_WireStringHeadingLevel(t *testing.T) {
	raw := `[{"heading":{"level":"heading-2","text":"一、背景"},"blockType":"heading","index":0,"id":"m1"},` +
		`{"paragraph":{"text":"公司在快速扩张"},"blockType":"paragraph","index":1,"id":"m2"},` +
		`{"heading":{"level":3,"text":"数字形态"},"blockType":"heading","index":2,"id":"m3"}]`
	blocks, err := extractBlocks(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("extractBlocks: %v", err)
	}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("blocksToMarkdown: %v", err)
	}
	want := "## 一、背景\n\n公司在快速扩张\n\n### 数字形态\n"
	if string(md) != want {
		t.Fatalf("markdown mismatch:\n got=%q\nwant=%q", string(md), want)
	}
}

func TestResourceTypeFor(t *testing.T) {
	if got := resourceTypeFor(wikiNode{Category: CategoryALIDOC}); got != "document" {
		t.Fatalf("ALIDOC should be document, got %q", got)
	}
	if got := resourceTypeFor(wikiNode{Category: "FILE", HasChildren: true}); got != "folder" {
		t.Fatalf("hasChildren should be folder, got %q", got)
	}
	if got := resourceTypeFor(wikiNode{Category: "FILE"}); got != "file" {
		t.Fatalf("plain file expected, got %q", got)
	}
}

func TestBlockElementJSONShape(t *testing.T) {
	// Lock the wire contract: discriminator + same-named property key.
	raw := `{"blockType":"heading","heading":{"text":"T","level":2},"id":"b1","index":3}`
	var b blockElement
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.BlockType != "heading" || b.Heading == nil || b.Heading.Level != 2 || b.Index != 3 {
		t.Fatalf("unexpected decoded block: %+v", b)
	}
}

func TestClientImplementsInterfaces(t *testing.T) {
	// The connector must satisfy both the base and streaming interfaces; the
	// service dispatches to FetchStream only if this assertion holds.
	var _ datasource.Connector = (*Connector)(nil)
	var _ datasource.StreamingConnector = (*Connector)(nil)
}

func TestBareDocIDRoundTrip(t *testing.T) {
	const uuid = "mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p"
	if got := makeBareDocID(uuid); got != "doc:"+uuid {
		t.Fatalf("makeBareDocID = %q", got)
	}
	if got := bareDocID(makeBareDocID(uuid)); got != uuid {
		t.Fatalf("bareDocID round trip = %q", got)
	}
	// Knowledge-base resource IDs must not be mistaken for bare docs.
	for _, rid := range []string{"", "doc:", "By8jQSoKDj1drD0M", "By8jQSoKDj1drD0M/nodeID"} {
		if got := bareDocID(rid); got != "" {
			t.Fatalf("bareDocID(%q) = %q, want empty", rid, got)
		}
	}
}

// The three URL shapes observed in the wild: a personal doc on docs.dingtalk.com,
// a sheet-style doc with a heavy query string, and a wiki node URL with utm
// parameters. All must yield the dentryUuid after /nodes/.
func TestDentryUUIDFromURL_RealWorldSamples(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://docs.dingtalk.com/i/nodes/mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p", "mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p"},
		{"https://docs.dingtalk.com/i/nodes/b9Y4gmKWrPz5zbOBijEDl1qyJGXn6lpz?iframeQuery=entrance%3Ddata%26sheetId%3DhERWDMS%26viewId%3DqvGDAH2", "b9Y4gmKWrPz5zbOBijEDl1qyJGXn6lpz"},
		{"https://alidocs.dingtalk.com/i/nodes/PwkYGxZV3ZmjmynKu3rbRRNEWAgozOKL?utm_scene=team_space", "PwkYGxZV3ZmjmynKu3rbRRNEWAgozOKL"},
	}
	for _, tc := range cases {
		if got := dentryUUIDFromURL(tc.url); got != tc.want {
			t.Fatalf("dentryUUIDFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestCursorDocHashesRoundTrip(t *testing.T) {
	c := &ddCursor{
		DocHashes: map[string]string{"mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p": "abc123"},
	}
	decoded := decodeCursor(encodeCursor(c))
	if got := decoded.DocHashes["mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p"]; got != "abc123" {
		t.Fatalf("doc hash lost in round trip: %+v", decoded.DocHashes)
	}
	if decoded.DocHashes == nil {
		t.Fatal("decoded.DocHashes must be non-nil")
	}
}

func TestDocTitleFromBlocks(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "正文"}},
		{BlockType: "heading", Heading: &headingProps{Text: "第一个标题"}},
	}
	if got := docTitleFromBlocks(blocks, "someKey123"); got != "第一个标题" {
		t.Fatalf("title = %q, want first heading", got)
	}
	if got := docTitleFromBlocks(nil, "mweZ92PV6MYXYy09FqZm2dgMWxEKBD6p"); got != "钉钉文档 mweZ92PV" {
		t.Fatalf("fallback title = %q", got)
	}
}

func TestApiStatusError(t *testing.T) {
	e := &apiStatusError{Status: 404, Body: `{"code":"not.found"}`}
	if got := e.Error(); got != `dingtalk API error status=404 body={"code":"not.found"}` {
		t.Fatalf("unexpected message %q", got)
	}
}
