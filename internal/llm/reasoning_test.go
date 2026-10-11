package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReasoningStaysOutOfAnswersAndTextTools(t *testing.T) {
	const action = `<tool_call>{"name":"create_file","arguments":{"content":"must not execute"}}</tool_call>`
	for _, test := range []struct {
		name, content, answer string
	}{
		{"plain", "Обычный ответ", "Обычный ответ"},
		{"closed", "<think>private</think>Ответ", "Ответ"},
		{"nested", "<think>private<analysis>nested</analysis>still private</think>Ответ", "Ответ"},
		{"unclosed", "<think>private", ""},
		{"unclosed after answer", "Ответ<analysis>private", "Ответ"},
		{"missing opening tag", "private</think>Ответ", "Ответ"},
		{"analysis channel", "<|channel|>analysis private", ""},
		{"final channel", "<|channel|>analysis private<|channel|>final Ответ", "Ответ"},
		{"legacy channel", "<channel|>analysis private<channel|>final Ответ", "Ответ"},
		{"case and whitespace", "<|CHANNEL|> ANALYSIS private<|CHANNEL|> FINAL Ответ", "Ответ"},
		{"final inside thought", "<think>private<|channel|>final still private", ""},
		{"framed final", "<|start|>assistant<|channel|>analysis private<|end|><|start|>assistant<|channel|>final<|message|>Ответ<|end|>", "Ответ"},
		{"thought action", "<think>consider " + action + "</think>Ответ", "Ответ"},
		{"unclosed thought action", "<think>consider " + action, ""},
		{"analysis channel action", "<|channel|>analysis consider " + action + "<|channel|>final Ответ", "Ответ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if answer := formatAnswer(test.content); answer != test.answer {
				t.Errorf("formatAnswer() = %q, want %q", answer, test.answer)
			}
			if calls := parseTextToolCalls(test.content, 0); len(calls) != 0 {
				t.Fatalf("reasoning caused tool execution: %#v", calls)
			}
		})
	}
}

func TestTextToolPreservesLiteralReasoningTagsInGeneratedCode(t *testing.T) {
	const code = "const tags = '<think>unfinished <analysis>inside</analysis> </think> <|channel|>analysis <|channel|>final';\n"
	arguments, err := json.Marshal(map[string]string{"content": code})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`<tool_call>{"name":"create_file","arguments":` + string(arguments) + `}</tool_call>`,
		`<tool_call>{"name":"create_file","arguments":` + strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(arguments)) + `}</tool_call>`,
		"<tool_call>create_file<arg_key>content</arg_key><arg_value>" + code + "</arg_value></tool_call>",
	} {
		content := "<think>consider <tool_call>current_time</tool_call></think>" + raw
		calls := parseTextToolCalls(content, 0)
		if len(calls) != 1 || calls[0].Function.Name != "create_file" {
			t.Fatalf("unexpected calls: %#v", calls)
		}
		if got := decodeArguments(calls[0].Function.Arguments)["content"]; got != code {
			t.Errorf("generated code changed: got %q, want %q", got, code)
		}
		if got := formatAnswer(content + "Готово"); got != "Готово" {
			t.Errorf("tool body leaked into answer: %q", got)
		}
	}
}
