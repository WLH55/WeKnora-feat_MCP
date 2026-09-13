package dingtalk

import (
	"strings"
	"testing"
)

// The block JSON below mirrors the documented wire shape:
// {"blockType":"x","x":{...}}. It is decoded the same way the API client does
// so the tests exercise the real struct tags.

func TestBlocksToMarkdown_ParagraphAndHeading(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "heading", Heading: &headingProps{Level: 2, Text: "标题"}},
		{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "正文内容"}},
	}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(md)
	want := "## 标题\n\n正文内容\n"
	if got != want {
		t.Fatalf("markdown mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestBlocksToMarkdown_HeadingLevelClamped(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 1}, {3, 3}, {9, 6}} {
		md, err := blocksToMarkdown([]blockElement{
			{BlockType: "heading", Heading: &headingProps{Level: flexInt(tc.in), Text: "x"}},
		})
		if err != nil {
			t.Fatalf("level %d: %v", tc.in, err)
		}
		wantPrefix := strings.Repeat("#", tc.want) + " x"
		if strings.TrimSpace(string(md)) != wantPrefix {
			t.Fatalf("level %d: got %q want %q", tc.in, strings.TrimSpace(string(md)), wantPrefix)
		}
	}
}

func TestBlocksToMarkdown_InlineFormatting(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "paragraph",
		Paragraph: &paragraphProps{
			Children: []inlineElement{
				{Text: &textProps{Text: "bold", Bold: true}},
				{Text: &textProps{Text: " "}},
				{Text: &textProps{Text: "italic", Italic: true}},
				{Text: &textProps{Text: " "}},
				{Text: &textProps{Text: "strike", Strike: true}},
				{Text: &textProps{Text: " "}},
				{Text: &textProps{Text: "under", Underline: true}},
			},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(string(md))
	want := "**bold** *italic* ~~strike~~ <u>under</u>"
	if got != want {
		t.Fatalf("inline mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestBlocksToMarkdown_LinkAndImage(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "paragraph",
		Paragraph: &paragraphProps{
			Children: []inlineElement{
				{Link: &linkProps{Href: "https://example.com", Children: []inlineElement{{Text: &textProps{Text: "示例"}}}}},
				{Text: &textProps{Text: " "}},
				{Image: &imageProps{Src: "https://img.example/a.png"}},
			},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(string(md))
	want := "[示例](https://example.com) ![](https://img.example/a.png)"
	if got != want {
		t.Fatalf("link/image mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestBlocksToMarkdown_LinkWithoutChildrenFallsBackToHref(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "paragraph",
		Paragraph: &paragraphProps{
			Children: []inlineElement{{Link: &linkProps{Href: "https://example.com"}}},
		},
	}}
	md, _ := blocksToMarkdown(blocks)
	if got := strings.TrimSpace(string(md)); got != "[https://example.com](https://example.com)" {
		t.Fatalf("got %q", got)
	}
}

func TestBlocksToMarkdown_Lists(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "unorderedList", UnorderedList: &listProps{List: &listObjectProps{Level: 0}, Text: "第一项"}},
		{BlockType: "unorderedList", UnorderedList: &listProps{List: &listObjectProps{Level: 1}, Text: "子项"}},
		{BlockType: "orderedList", OrderedList: &listProps{List: &listObjectProps{Level: 0}, Text: "有序项"}},
	}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(md)
	for _, want := range []string{"- 第一项", "  - 子项", "1. 有序项"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in:\n%s", want, got)
		}
	}
}

func TestBlocksToMarkdown_BlockquoteMultiline(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "blockquote", Blockquote: &quoteProps{Text: "引用一"}},
	}
	md, _ := blocksToMarkdown(blocks)
	if got := strings.TrimSpace(string(md)); got != "> 引用一" {
		t.Fatalf("got %q", got)
	}
}

func TestBlocksToMarkdown_CalloutWithNestedBlocks(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "callout",
		Callout: &calloutProps{
			Sticker: "💡",
			Children: []blockElement{
				{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "提示内容"}},
			},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.TrimSpace(string(md))
	if !strings.HasPrefix(got, "> 💡") || !strings.Contains(got, "提示内容") {
		t.Fatalf("callout not rendered as quoted block with sticker: %q", got)
	}
}

func TestBlocksToMarkdown_ColumnsFlattened(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "columns",
		Columns: &columnsProps{
			Size: 2,
			Children: []blockElement{
				{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "左栏"}},
				{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "右栏"}},
			},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(md)
	if !strings.Contains(got, "左栏") || !strings.Contains(got, "右栏") {
		t.Fatalf("columns children dropped: %q", got)
	}
}

func TestBlocksToMarkdown_TableFromCells(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "table",
		Table: &tableProps{
			RowSize: 2,
			ColSize: 2,
			Cells:   [][]string{{"A", "B"}, {"a", "b"}},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(md)
	want := "| A | B |\n| --- | --- |\n| a | b |"
	if !strings.Contains(got, want) {
		t.Fatalf("table mismatch:\n got=%q\nwant contains=%q", got, want)
	}
}

func TestBlocksToMarkdown_TablePipesEscaped(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "table",
		Table:     &tableProps{Cells: [][]string{{"a|b", "c"}, {"d", "e"}}},
	}}
	md, _ := blocksToMarkdown(blocks)
	if !strings.Contains(string(md), `a\|b`) {
		t.Fatalf("pipe not escaped: %q", string(md))
	}
}

func TestBlocksToMarkdown_TableFromRowsFallback(t *testing.T) {
	blocks := []blockElement{{
		BlockType: "table",
		Table: &tableProps{
			Children: []blockElement{
				{BlockType: "tableRow", TableRow: &tableRowProps{Cells: []string{"H1", "H2"}}},
				{BlockType: "tableRow", TableRow: &tableRowProps{Cells: []string{"v1", "v2"}}},
			},
		},
	}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(md); !strings.Contains(got, "| H1 | H2 |") || !strings.Contains(got, "| v1 | v2 |") {
		t.Fatalf("row fallback failed: %q", got)
	}
}

func TestBlocksToMarkdown_UnknownBlockIgnored(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "somethingNew", ID: "x"},
		{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "保留"}},
	}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unknown block must not fail the document: %v", err)
	}
	if got := string(md); !strings.Contains(got, "保留") {
		t.Fatalf("known block dropped: %q", got)
	}
}

func TestBlocksToMarkdown_BlockTypeInferredWhenAbsent(t *testing.T) {
	// No blockType discriminator: the renderer infers the kind from the
	// populated property pointer, so content is still rendered.
	blocks := []blockElement{{Paragraph: &paragraphProps{Text: "推断"}}}
	md, err := blocksToMarkdown(blocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(md)); got != "推断" {
		t.Fatalf("got %q", got)
	}
}

func TestBlocksToMarkdown_EmptyParagraphSkipped(t *testing.T) {
	blocks := []blockElement{
		{BlockType: "paragraph", Paragraph: &paragraphProps{}},
		{BlockType: "paragraph", Paragraph: &paragraphProps{Text: "有内容"}},
	}
	md, _ := blocksToMarkdown(blocks)
	if got := string(md); strings.HasPrefix(got, "\n\n") {
		t.Fatalf("empty block produced leading blank lines: %q", got)
	}
}
