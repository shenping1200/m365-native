package web

import (
	"encoding/json"
	"strings"
)

// estimateTokens approximates token usage for ChatHub-backed responses. The
// upstream ChatHub protocol never reports token counts, so the gateway must
// estimate them for protocol compatibility. new-api (Anthropic relay format)
// bills directly from the usage values in this response, so non-zero estimates
// are required for a request to be treated as successful at all.
//
// Heuristic: CJK runes count as one token each; other characters average
// ~4 per token; each whitespace/newline run adds one token.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var total, cjk, wsGroups int
	prevWS := true
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF || r >= 0xF900 && r <= 0xFAFF || r >= 0x3040 && r <= 0x30FF {
			cjk++
			prevWS = false
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevWS {
				wsGroups++
			}
			prevWS = true
			continue
		}
		total++
		prevWS = false
	}
	t := cjk + (total+3)/4 + wsGroups
	if t < 1 {
		return 1
	}
	return t
}

// estimateInputTokens adds a small per-message/per-tool overhead so a
// multi-turn or tool-bearing request is not charged as if it were a single
// bare line of text.
func estimateInputTokens(prompt string, messages int, tools int) int {
	base := estimateTokens(prompt)
	if messages > 1 {
		base += (messages - 1) * 4
	}
	if tools > 0 {
		base += tools * 8
	}
	return base
}

// openAIUsage builds the OpenAI-style usage object used by
// /v1/chat/completions (non-streaming) and the final SSE chunk (streaming).
func openAIUsage(inputText, outputText string) map[string]any {
	in := estimateTokens(inputText)
	out := estimateTokens(outputText)
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
}

// anthropicUsage builds the Anthropic-style usage object used by
// /v1/messages message_start / message_delta / non-streaming responses.
func anthropicUsage(inputText, outputText string) map[string]any {
	return map[string]any{
		"input_tokens":  estimateTokens(inputText),
		"output_tokens": estimateTokens(outputText),
	}
}

// outputTextOfBlocks concatenates the generated text payload of Anthropic
// content blocks (text plus tool_use input JSON) for token estimation.
func outputTextOfBlocks(blocks []any) string {
	var b strings.Builder
	for _, raw := range blocks {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "text":
			if s, _ := m["text"].(string); s != "" {
				b.WriteString(s)
			}
		case "tool_use":
			if input, ok := m["input"]; ok {
				if j, err := json.Marshal(input); err == nil {
					b.Write(j)
				}
			}
		}
	}
	return b.String()
}

// usageFromOpenAISource extracts an already-computed usage object from an
// OpenAI-style response map (produced by openaiChat), falling back to an
// estimate derived from the given text when the source carries none.
func usageFromOpenAISource(src map[string]any, fallbackOutput string) map[string]any {
	if raw, ok := src["usage"].(map[string]any); ok {
		in, _ := raw["prompt_tokens"].(float64)
		out, _ := raw["completion_tokens"].(float64)
		return map[string]any{
			"input_tokens":  int(in),
			"output_tokens": int(out),
		}
	}
	return anthropicUsage("", fallbackOutput)
}
