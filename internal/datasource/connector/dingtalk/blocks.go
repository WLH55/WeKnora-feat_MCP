package dingtalk

import (
	"fmt"
	"strings"
)

// blocksToMarkdown renders a DingTalk online document's block-element tree to
// Markdown. It is the counterpart of the Feishu core renderer
// (feishu/core/markdown.go) but for DingTalk's block vocabulary.
//
// The function is intentionally tolerant: unknown block/inline types render to
// an empty string rather than failing the whole document, so a document that
// uses a newer block type still syncs with its remaining content intact.
func blocksToMarkdown(blocks []blockElement) ([]byte, error) {
	body, err := renderBlocks(blocks)
	if err != nil {
		return nil, err
	}
	return []byte(body + "\n"), nil
}

// renderBlocks renders a sequence of blocks separated by blank lines.
func renderBlocks(blocks []blockElement) (string, error) {
	parts := make([]string, 0, len(blocks))
	for i, b := range blocks {
		s, err := renderBlock(b)
		if err != nil {
			return "", fmt.Errorf("block %d (%s): %w", i, blockTypeOf(b), err)
		}
		if strings.TrimSpace(s) == "" {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n"), nil
}

func renderBlock(b blockElement) (string, error) {
	switch blockTypeOf(b) {
	case "paragraph":
		if b.Paragraph == nil {
			return "", nil
		}
		return inlineOrText(b.Paragraph.Children, b.Paragraph.Text), nil

	case "heading":
		if b.Heading == nil {
			return "", nil
		}
		level := b.Heading.Level
		if level < 1 {
			level = 1
		}
		if level > 6 {
			level = 6
		}
		text := inlineOrText(b.Heading.Children, b.Heading.Text)
		if strings.TrimSpace(text) == "" {
			return "", nil
		}
		return strings.Repeat("#", level) + " " + text, nil

	case "blockquote":
		if b.Blockquote == nil {
			return "", nil
		}
		return quoteLines(inlineOrText(b.Blockquote.Children, b.Blockquote.Text)), nil

	case "callout":
		if b.Callout == nil {
			return "", nil
		}
		return renderCallout(b.Callout)

	case "columns":
		if b.Columns == nil {
			return "", nil
		}
		// Columns have no Markdown equivalent; flatten children in order.
		return renderBlocks(b.Columns.Children)

	case "orderedList":
		if b.OrderedList == nil {
			return "", nil
		}
		return renderList(b.OrderedList, "1."), nil

	case "unorderedList":
		if b.UnorderedList == nil {
			return "", nil
		}
		return renderList(b.UnorderedList, "-"), nil

	case "table":
		if b.Table == nil {
			return "", nil
		}
		return renderTable(b.Table), nil

	case "tableRow", "tableCell":
		// These normally appear only as table children. If one surfaces at the
		// top level, render its text so content is not silently dropped.
		return renderStandaloneTablePart(b), nil

	default:
		return "", nil
	}
}

// blockTypeOf returns the discriminator, falling back to inferring the kind
// from whichever property pointer is populated (in case blockType is absent).
func blockTypeOf(b blockElement) string {
	if b.BlockType != "" {
		return b.BlockType
	}
	switch {
	case b.Paragraph != nil:
		return "paragraph"
	case b.Heading != nil:
		return "heading"
	case b.Blockquote != nil:
		return "blockquote"
	case b.Callout != nil:
		return "callout"
	case b.Columns != nil:
		return "columns"
	case b.OrderedList != nil:
		return "orderedList"
	case b.UnorderedList != nil:
		return "unorderedList"
	case b.Table != nil:
		return "table"
	case b.TableRow != nil:
		return "tableRow"
	case b.TableCell != nil:
		return "tableCell"
	}
	return ""
}

// inlineOrText prefers rendered inline children (they carry formatting) and
// falls back to the block's plain `text` field.
func inlineOrText(children []inlineElement, text string) string {
	rendered := renderInline(children)
	if strings.TrimSpace(rendered) == "" {
		return text
	}
	return rendered
}

// renderInline concatenates inline elements into a Markdown fragment.
func renderInline(children []inlineElement) string {
	if len(children) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range children {
		sb.WriteString(renderInlineElement(c))
	}
	return sb.String()
}

func renderInlineElement(c inlineElement) string {
	switch {
	case c.Text != nil:
		return formatText(c.Text)
	case c.Link != nil:
		label := renderInline(c.Link.Children)
		if strings.TrimSpace(label) == "" {
			label = c.Link.Href
		}
		if c.Link.Href == "" {
			return label
		}
		return fmt.Sprintf("[%s](%s)", label, c.Link.Href)
	case c.Image != nil:
		if c.Image.Src == "" {
			return ""
		}
		return fmt.Sprintf("![](%s)", c.Image.Src)
	case c.Sticker != nil:
		// Sticker codes are DingTalk's emoji identifiers/labels. They are
		// emitted verbatim; dropping them would lose the only signal present.
		return c.Sticker.Code
	default:
		return ""
	}
}

// formatText applies inline emphasis. Markdown has no underline, so it is
// rendered as an HTML span, which WeKnora's Markdown pipeline preserves.
func formatText(t *textProps) string {
	s := t.Text
	if s == "" {
		return ""
	}
	if t.Bold {
		s = "**" + s + "**"
	}
	if t.Italic {
		s = "*" + s + "*"
	}
	if t.Strike {
		s = "~~" + s + "~~"
	}
	if t.Underline {
		s = "<u>" + s + "</u>"
	}
	return s
}

// renderList renders an ordered/unordered list item. DingTalk nests lists via
// `list.level` (0-based), so indentation is derived from it.
func renderList(l *listProps, marker string) string {
	text := inlineOrText(l.Children, l.Text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	level := 0
	if l.List != nil && l.List.Level > 0 {
		level = l.List.Level
	}
	indent := strings.Repeat("  ", level)
	return indent + marker + " " + text
}

// renderCallout renders a callout (高亮块) as a blockquote so its nested blocks
// keep their structure while remaining visually distinct from body text.
func renderCallout(c *calloutProps) (string, error) {
	inner, err := renderBlocks(c.Children)
	if err != nil {
		return "", err
	}
	if c.Sticker != "" {
		if strings.TrimSpace(inner) == "" {
			inner = c.Sticker
		} else {
			inner = c.Sticker + " " + inner
		}
	}
	return quoteLines(inner), nil
}

// quoteLines prefixes every line with "> ". Empty lines become a bare ">".
func quoteLines(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + ln
	}
	return strings.Join(lines, "\n")
}

// renderTable renders a table block. DingTalk documents return the full grid in
// `cells` (String[][]) in the common case; when that is absent the renderer
// falls back to tableRow/tableCell children.
func renderTable(t *tableProps) string {
	rows := t.Cells
	if len(rows) == 0 {
		rows = tableRowsFromChildren(t.Children)
	}
	return markdownTable(rows)
}

func tableRowsFromChildren(children []blockElement) [][]string {
	var rows [][]string
	for _, ch := range children {
		if ch.TableRow == nil {
			continue
		}
		row := ch.TableRow.Cells
		if len(row) == 0 {
			var built []string
			for _, cell := range ch.TableRow.Children {
				if cell.TableCell != nil {
					built = append(built, cellText(cell.TableCell))
				}
			}
			row = built
		}
		rows = append(rows, row)
	}
	return rows
}

// renderStandaloneTablePart renders a tableRow/tableCell that appears outside a
// table (defensive; keeps its text content instead of dropping it).
func renderStandaloneTablePart(b blockElement) string {
	if b.TableCell != nil {
		return cellText(b.TableCell)
	}
	if b.TableRow != nil {
		rows := tableRowsFromChildren([]blockElement{b})
		if len(rows) == 1 {
			return strings.Join(rows[0], " | ")
		}
	}
	return ""
}

func cellText(c *tableCellProps) string {
	if c == nil {
		return ""
	}
	if s := renderInline(c.Children); strings.TrimSpace(s) != "" {
		return s
	}
	if len(c.Blocks) > 0 {
		if s, err := renderBlocks(c.Blocks); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return c.Text
}

// markdownTable builds a GitHub-flavored Markdown table. The first row is used
// as the header (DingTalk documents have no separate header concept).
func markdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}

	pad := func(r []string) []string {
		out := make([]string, cols)
		for i := range out {
			if i < len(r) {
				out[i] = escapePipe(strings.ReplaceAll(r[i], "\n", " "))
			}
		}
		return out
	}

	var sb strings.Builder
	sb.WriteString("| " + strings.Join(pad(rows[0]), " | ") + " |")
	seps := make([]string, cols)
	for i := range seps {
		seps[i] = "---"
	}
	sb.WriteString("\n| " + strings.Join(seps, " | ") + " |")
	for _, r := range rows[1:] {
		sb.WriteString("\n| " + strings.Join(pad(r), " | ") + " |")
	}
	return sb.String()
}

func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
