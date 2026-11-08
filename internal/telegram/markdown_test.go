package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFormatMarkdownPreservesRoleplayAndRendersStyles(t *testing.T) {
	input := "*грозно щёлкает* **важно** _курсив_ ~~старое~~ `a * b ** c` :3 <3\n>///<\n>_<"
	messages := FormatMarkdown(input, 4096)
	if len(messages) != 1 {
		t.Fatalf("messages=%d", len(messages))
	}
	message := messages[0]
	want := "*грозно щёлкает* важно курсив старое a * b ** c :3 <3\n\n>///<\n\n>_<"
	// Block boundaries around a leading > emoticon may add an empty line;
	// the literal emoticons and their angle brackets must remain untouched.
	if strings.ReplaceAll(message.Text, "\n\n", "\n") != strings.ReplaceAll(want, "\n\n", "\n") {
		t.Fatalf("text=%q", message.Text)
	}
	wanted := map[string]string{"bold": "важно", "italic": "курсив", "strikethrough": "старое", "code": "a * b ** c"}
	if len(message.Entities) != len(wanted) {
		t.Fatalf("entities=%#v", message.Entities)
	}
	for _, entity := range message.Entities {
		if got := markdownEntityText(message, entity); got != wanted[entity.Type] {
			t.Fatalf("entity=%#v text=%q", entity, got)
		}
	}
	for _, input := range []string{"*action*", "***literal***", "**bold *action* bold**", "*action **bold** action*"} {
		messages := FormatMarkdown(input, 4096)
		if strings.Count(messages[0].Text, "*") != strings.Count(strings.ReplaceAll(input, "**", ""), "*") && input != "***literal***" {
			t.Fatalf("single stars changed: input=%q output=%q", input, messages[0].Text)
		}
		if input == "***literal***" && messages[0].Text != input {
			t.Fatalf("ambiguous odd delimiter run changed: %q", messages[0].Text)
		}
	}
}

func TestFormatMarkdownKeepsCodeLiteralAndEmojiOffsets(t *testing.T) {
	code := "  const x = '<tag> & **bold**';  \n\tconsole.log('😀', x);\n\n"
	messages := FormatMarkdown("😀 **yes**\n\n```js\n"+code+"```\n\nafter", 4096)
	message := messages[0]
	if message.Text != "😀 yes\n\n"+code+"after" {
		t.Fatalf("text=%q", message.Text)
	}
	if len(message.Entities) != 2 || message.Entities[0].Type != "bold" || message.Entities[0].Offset != 3 || message.Entities[0].Length != 3 {
		t.Fatalf("emoji shifted entities: %#v", message.Entities)
	}
	pre := message.Entities[1]
	if pre.Type != "pre" || pre.Language != "js" || markdownEntityText(message, pre) != code {
		t.Fatalf("pre=%#v body=%q", pre, markdownEntityText(message, pre))
	}
	chunks := FormatMarkdown("```js\n"+strings.Repeat(code, 40)+"```", 47)
	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(chunk.Text)
		if length := len(utf16.Encode([]rune(chunk.Text))); length > 47 {
			t.Fatalf("chunk length=%d", length)
		}
		if len(chunk.Entities) != 1 || chunk.Entities[0].Type != "pre" || chunk.Entities[0].Offset != 0 || chunk.Entities[0].Language != "js" || markdownEntityText(chunk, chunk.Entities[0]) != chunk.Text {
			t.Fatalf("lost pre formatting: %#v", chunk)
		}
	}
	if output.String() != strings.Repeat(code, 40) {
		t.Fatal("splitting changed code whitespace")
	}
}

func TestFormatMarkdownLinksListsHTMLAndEscapes(t *testing.T) {
	input := "# Heading\n\n3. **first**\n4. [site](https://example.com/?a=1&b=2)\n   - `nested`\n\n<img src=x> &amp; \\*literal\\*\n\n[unsafe](javascript:alert(1))"
	messages := FormatMarkdown(input, 4096)
	message := messages[0]
	for _, expected := range []string{"Heading", "3. first", "4. site", "  • nested", "<img src=x> & *literal*", "unsafe"} {
		if !strings.Contains(message.Text, expected) {
			t.Errorf("missing %q in %q", expected, message.Text)
		}
	}
	links := 0
	for _, entity := range message.Entities {
		if entity.Type == "text_link" {
			links++
			if entity.URL != "https://example.com/?a=1&b=2" || markdownEntityText(message, entity) != "site" {
				t.Fatalf("link=%#v", entity)
			}
		}
	}
	if links != 1 {
		t.Fatalf("links=%d", links)
	}
}

func TestFormatMarkdownProducesLegalOverlaps(t *testing.T) {
	for _, input := range []string{
		"> prefix **before [link](https://example.com) after** suffix",
		"> **bold `code` and _italic_**\n>\n> ```go\n> x := 1\n> ```\n>\n> [a `b` c](https://example.com)",
		"> outer\n> > inner **bold**\n> end",
		"**before _inside `code` italic_ after**",
	} {
		for _, message := range FormatMarkdown(input, 17) {
			units := len(utf16.Encode([]rune(message.Text)))
			for i, a := range message.Entities {
				if a.Offset < 0 || a.Length <= 0 || a.Offset+a.Length > units {
					t.Fatalf("out-of-bounds entity %#v in %#v", a, message)
				}
				for _, b := range message.Entities[i+1:] {
					if a.Offset >= b.Offset+b.Length || b.Offset >= a.Offset+a.Length {
						continue
					}
					if a.Type == "pre" || a.Type == "code" || b.Type == "pre" || b.Type == "code" {
						t.Fatalf("code overlap: %#v %#v", a, b)
					}
					if !markdownStyle(a.Type) && !markdownStyle(b.Type) {
						t.Fatalf("structural overlap: %#v %#v", a, b)
					}
					if !(a.Offset <= b.Offset && a.Offset+a.Length >= b.Offset+b.Length) && !(b.Offset <= a.Offset && b.Offset+b.Length >= a.Offset+a.Length) {
						t.Fatalf("partial overlap: %#v %#v for %q", a, b, input)
					}
				}
			}
		}
	}
}

func TestFormatMarkdownMalformedAndPlainText(t *testing.T) {
	for _, input := range []string{"hello\nworld", "unfinished **bold", "a_b_c", "x < y && y > z", "unclosed `code", "*action*"} {
		messages := FormatMarkdown(input, 4096)
		if len(messages) != 1 || messages[0].Text != input || len(messages[0].Entities) != 0 {
			t.Fatalf("input=%q output=%#v", input, messages)
		}
	}
	if messages := FormatMarkdown(" \n\t ", 4096); len(messages) != 0 {
		t.Fatalf("blank output=%#v", messages)
	}
}

func markdownEntityText(message FormattedMessage, entity MessageEntity) string {
	units := utf16.Encode([]rune(message.Text))
	return string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
}

func markdownStyle(kind string) bool {
	return kind == "bold" || kind == "italic" || kind == "strikethrough"
}
