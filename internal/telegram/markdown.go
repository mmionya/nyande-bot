package telegram

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// FormattedMessage contains plain text and Telegram's UTF-16 entity ranges.
// It deliberately does not use parse_mode: code and ordinary <, > and & remain literal.
type FormattedMessage struct {
	Text     string
	Entities []MessageEntity
}

// FormatMarkdown renders CommonMark and strikethrough, then splits the visible
// text without dropping whitespace from code or cutting a UTF-16 surrogate pair.
// A limit below two defaults to Telegram's normal message limit.
func FormatMarkdown(value string, limit int) []FormattedMessage {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if limit < 2 {
		limit = 4096
	}
	source := []byte(strings.ToValidUTF8(value, "\uFFFD"))
	markdown := goldmark.New(
		goldmark.WithExtensions(extension.Strikethrough),
		goldmark.WithParserOptions(parser.WithInlineParsers(util.Prioritized(literalStarParser{}, 450))),
	)
	r := markdownRenderer{source: source}
	r.blocks(markdown.Parser().Parse(text.NewReader(source)), 0, 0)
	r.entities = normalizeMarkdownEntities(r.entities)
	return splitFormattedText(r.output.String(), r.entities, limit)
}

// Single asterisks are used for roleplay actions by the bot. Consume them before
// CommonMark's emphasis parser; double asterisks still produce bold text. Odd
// longer runs are left literal rather than silently removing their single star.
type literalStarParser struct{}

func (literalStarParser) Trigger() []byte { return []byte{'*'} }

func (literalStarParser) Parse(_ ast.Node, reader text.Reader, _ parser.Context) ast.Node {
	line, segment := reader.PeekLine()
	count := 0
	for count < len(line) && line[count] == '*' {
		count++
	}
	if count%2 == 0 {
		return nil
	}
	reader.Advance(count)
	return ast.NewTextSegment(segment.WithStop(segment.Start + count))
}

type markdownRenderer struct {
	source   []byte
	output   strings.Builder
	units    int
	entities []MessageEntity
}

func (r *markdownRenderer) write(value string) {
	r.output.WriteString(value)
	for _, char := range value {
		r.units++
		if char > 0xffff {
			r.units++
		}
	}
}

func (r *markdownRenderer) separate(lines int) {
	value := r.output.String()
	if len(value) == 0 {
		return
	}
	count := 0
	for i := len(value) - 1; i >= 0 && value[i] == '\n'; i-- {
		count++
	}
	if count < lines {
		r.write(strings.Repeat("\n", lines-count))
	}
}

func (r *markdownRenderer) entity(kind string, start int, destination, language string) {
	if r.units > start {
		r.entities = append(r.entities, MessageEntity{
			Type: kind, Offset: start, Length: r.units - start, URL: destination, Language: language,
		})
	}
}

func (r *markdownRenderer) blocks(parent ast.Node, depth, quoteDepth int) {
	first := true
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		if !first {
			r.separate(2)
		}
		r.block(node, depth, quoteDepth)
		first = false
	}
}

func (r *markdownRenderer) block(node ast.Node, depth, quoteDepth int) {
	switch n := node.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		r.inlines(node)
	case *ast.Heading:
		start := r.units
		r.inlines(node)
		r.entity("bold", start, "", "")
	case *ast.FencedCodeBlock:
		start := r.units
		r.write(string(n.Lines().Value(r.source)))
		r.entity("pre", start, "", codeLanguage(n.Language(r.source)))
	case *ast.CodeBlock:
		start := r.units
		r.write(string(n.Lines().Value(r.source)))
		r.entity("pre", start, "", "")
	case *ast.Blockquote:
		// Common chat emoticons at the start of a line are not quotations.
		suffix := string(r.source[n.Pos():])
		if strings.HasPrefix(suffix, ">///<") || strings.HasPrefix(suffix, ">_<") || strings.HasPrefix(suffix, ">.<") {
			end := n.Pos()
			_ = ast.Walk(n, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
				if entering && child.Type() == ast.TypeBlock && child.Lines().Len() > 0 {
					end = max(end, child.Lines().At(child.Lines().Len()-1).Stop)
				}
				return ast.WalkContinue, nil
			})
			r.write(string(r.source[n.Pos():end]))
			return
		}
		start := r.units
		r.blocks(n, depth, quoteDepth+1)
		if quoteDepth == 0 {
			r.entity("blockquote", start, "", "")
		}
	case *ast.List:
		index := n.Start
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			if item != n.FirstChild() {
				r.separate(1)
			}
			r.write(strings.Repeat("  ", depth))
			if n.IsOrdered() {
				r.write(strconv.Itoa(index) + ". ")
				index++
			} else {
				r.write("• ")
			}
			for child := item.FirstChild(); child != nil; child = child.NextSibling() {
				if child != item.FirstChild() {
					r.separate(1)
				}
				r.block(child, depth+1, quoteDepth)
			}
		}
	case *ast.HTMLBlock:
		r.write(string(n.Lines().Value(r.source)))
		if n.HasClosure() {
			r.write(string(n.ClosureLine.Value(r.source)))
		}
	case *ast.ThematicBreak:
		r.write("────")
	default:
		r.blocks(node, depth, quoteDepth)
	}
}

func (r *markdownRenderer) inlines(parent ast.Node) {
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		start := r.units
		switch n := node.(type) {
		case *ast.Text:
			value := n.Segment.Value(r.source)
			if !n.IsRaw() {
				value = markdownText(value)
			}
			r.write(string(value))
			if n.SoftLineBreak() || n.HardLineBreak() {
				r.write("\n")
			}
		case *ast.String:
			value := n.Value
			if !n.IsRaw() && !n.IsCode() {
				value = markdownText(value)
			}
			r.write(string(value))
		case *ast.CodeSpan:
			for child := n.FirstChild(); child != nil; child = child.NextSibling() {
				segment := child.(*ast.Text).Segment
				value := string(segment.Value(r.source))
				if strings.HasSuffix(value, "\n") {
					value = strings.TrimSuffix(value, "\n") + " "
				}
				r.write(value)
			}
			r.entity("code", start, "", "")
		case *ast.Emphasis:
			r.inlines(n)
			kind := "italic"
			if n.Level == 2 {
				kind = "bold"
			}
			r.entity(kind, start, "", "")
		case *extast.Strikethrough:
			r.inlines(n)
			r.entity("strikethrough", start, "", "")
		case *ast.Link:
			r.inlines(n)
			if destination := markdownLink(n.Destination); destination != "" {
				r.entity("text_link", start, destination, "")
			}
		case *ast.Image:
			r.inlines(n)
			destination := markdownLink(n.Destination)
			if r.units == start {
				r.write(string(markdownText(n.Destination)))
			}
			if destination != "" {
				r.entity("text_link", start, destination, "")
			}
		case *ast.AutoLink:
			r.write(string(n.Label(r.source)))
			destination := n.URL(r.source)
			if n.AutoLinkType == ast.AutoLinkEmail {
				destination = append([]byte("mailto:"), destination...)
			}
			if destination := markdownLink(destination); destination != "" {
				r.entity("text_link", start, destination, "")
			}
		case *ast.RawHTML:
			for index := 0; index < n.Segments.Len(); index++ {
				segment := n.Segments.At(index)
				r.write(string(segment.Value(r.source)))
			}
		default:
			r.inlines(node)
		}
	}
}

func markdownText(value []byte) []byte {
	return util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(value)))
}

func markdownLink(value []byte) string {
	destination := string(util.URLEscape(value, true))
	parsed, err := url.Parse(destination)
	if err != nil || strings.ContainsAny(destination, "\r\n\x00") {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http":
		if parsed.Host == "" {
			return ""
		}
	case "mailto":
		if parsed.Opaque == "" {
			return ""
		}
	case "tg":
		if parsed.Host == "" {
			return ""
		}
	default:
		return ""
	}
	return destination
}

func codeLanguage(value []byte) string {
	language := string(markdownText(value))
	if len(language) > 64 {
		return ""
	}
	for _, char := range language {
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) && !strings.ContainsRune("_+-.#", char) {
			return ""
		}
	}
	return language
}

// Telegram forbids any formatting inside code/pre, nested quotes, and nesting
// links and quotes. Keep code intact and split surrounding entities around it.
func normalizeMarkdownEntities(entities []MessageEntity) []MessageEntity {
	priority := func(kind string) int {
		switch kind {
		case "pre", "code":
			return 0
		case "text_link":
			return 1
		case "blockquote":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(entities, func(i, j int) bool { return priority(entities[i].Type) < priority(entities[j].Type) })
	result := make([]MessageEntity, 0, len(entities))
	for _, entity := range entities {
		pieces := []MessageEntity{entity}
		for _, previous := range result {
			if priority(previous.Type) == 3 || (priority(entity.Type) == 3 && priority(previous.Type) != 0) {
				continue
			}
			var remaining []MessageEntity
			for _, piece := range pieces {
				start, end := piece.Offset, piece.Offset+piece.Length
				cutStart, cutEnd := previous.Offset, previous.Offset+previous.Length
				if cutStart >= end || cutEnd <= start {
					remaining = append(remaining, piece)
					continue
				}
				if start < cutStart {
					before := piece
					before.Length = cutStart - start
					remaining = append(remaining, before)
				}
				if end > cutEnd {
					after := piece
					after.Offset, after.Length = cutEnd, end-cutEnd
					remaining = append(remaining, after)
				}
			}
			pieces = remaining
		}
		result = append(result, pieces...)
	}
	// Splitting a quote around a link can leave an enclosing style crossing a
	// quote boundary. Split styles at every structural boundary as well, so all
	// remaining overlaps are complete containment, as required by Telegram.
	var boundaries []int
	for _, entity := range result {
		if priority(entity.Type) < 3 {
			boundaries = append(boundaries, entity.Offset, entity.Offset+entity.Length)
		}
	}
	sort.Ints(boundaries)
	var normalized []MessageEntity
	for _, entity := range result {
		if priority(entity.Type) < 3 {
			normalized = append(normalized, entity)
			continue
		}
		end := entity.Offset + entity.Length
		for _, boundary := range boundaries {
			if boundary > entity.Offset && boundary < end {
				piece := entity
				piece.Length = boundary - entity.Offset
				normalized = append(normalized, piece)
				entity.Offset = boundary
			}
		}
		entity.Length = end - entity.Offset
		normalized = append(normalized, entity)
	}
	result = normalized
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Offset == result[j].Offset {
			return result[i].Length > result[j].Length
		}
		return result[i].Offset < result[j].Offset
	})
	return result
}

func splitFormattedText(value string, entities []MessageEntity, limit int) []FormattedMessage {
	var result []FormattedMessage
	startByte, startUnits := 0, 0
	for startByte < len(value) {
		endByte, units := startByte, 0
		lineBreak, wordBreak := -1, -1
		for endByte < len(value) {
			char, size := utf8.DecodeRuneInString(value[endByte:])
			width := 1
			if char > 0xffff {
				width = 2
			}
			if units+width > limit {
				break
			}
			endByte += size
			units += width
			if units >= limit/2 && char == '\n' {
				lineBreak = endByte
			}
			if units >= limit/2 && unicode.IsSpace(char) {
				wordBreak = endByte
			}
		}
		if endByte < len(value) {
			if lineBreak > startByte {
				endByte = lineBreak
			} else if wordBreak > startByte {
				endByte = wordBreak
			}
			units = 0
			for _, char := range value[startByte:endByte] {
				units++
				if char > 0xffff {
					units++
				}
			}
		}
		message := FormattedMessage{Text: value[startByte:endByte]}
		for _, entity := range entities {
			start := max(startUnits, entity.Offset)
			end := min(startUnits+units, entity.Offset+entity.Length)
			if start < end {
				entity.Offset, entity.Length = start-startUnits, end-start
				message.Entities = append(message.Entities, entity)
			}
		}
		result = append(result, message)
		startByte, startUnits = endByte, startUnits+units
	}
	return result
}
