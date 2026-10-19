// Inline thinking-tag extraction for OpenAI-compatible providers.
//
// Reasoning models served through relays (DeepSeek R1 distills, Qwen3,
// MiMo, ...) sometimes fold their chain of thought into the assistant
// content between inline thinking tags instead of exposing it via the
// structured reasoning_content field. Showing those blocks as chat
// output is noise, so the provider layer strips them and routes the
// captured text into the reasoning payload instead, where the agent
// treats it like any other chain of thought (separate reasoning
// channel, suppression via thinking_level, replay rules).
package openai_compat

import (
	"regexp"
	"strings"
)

// inlineThinkTag is one accepted inline reasoning tag spelling.
type inlineThinkTag struct {
	open  string
	close string
}

// closeOf derives the closing tag from the opening one so the two can
// never drift apart: "<think>" -> "</think>".
func closeOf(open string) string {
	if strings.HasPrefix(open, "<") && strings.HasSuffix(open, ">") {
		return "</" + open[1:]
	}
	return ""
}

var inlineThinkVariants = func() []inlineThinkTag {
	// think: DeepSeek R1 distills and Qwen3; thought: GLM relays;
	// reasoning: generic OpenRouter-style relays.
	opens := []string{"<think>", "<thought>", "<reasoning>"}
	sets := make([]inlineThinkTag, 0, len(opens))
	for _, open := range opens {
		sets = append(sets, inlineThinkTag{open: open, close: closeOf(open)})
	}
	return sets
}()

// selfClosingThinkTag is emitted by some relays for empty thoughts.
const selfClosingThinkTag = "<think/>"

var inlineThinkPairPatterns = func() []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(inlineThinkVariants))
	for _, variant := range inlineThinkVariants {
		if variant.close == "" {
			continue
		}
		patterns = append(patterns, regexp.MustCompile(
			"(?is)"+regexp.QuoteMeta(variant.open)+"(.*?)"+regexp.QuoteMeta(variant.close),
		))
	}
	return patterns
}()

// SplitInlineThink removes inline reasoning blocks from model output and
// returns the cleaned content together with the captured reasoning text.
// It tolerates paired blocks, self-closing tags, unclosed opening tags
// (streams truncated mid-thought) and stray closing tags.
func SplitInlineThink(content string) (clean string, think string) {
	if !strings.Contains(content, "<") {
		return content, ""
	}

	var caught strings.Builder
	appendThought := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if caught.Len() > 0 {
			caught.WriteString("\n\n")
		}
		caught.WriteString(text)
	}

	clean = content
	altered := false
	matched := true
	for matched {
		matched = false
		for _, pattern := range inlineThinkPairPatterns {
			found := pattern.FindStringSubmatchIndex(clean)
			if found == nil {
				continue
			}
			appendThought(clean[found[2]:found[3]])
			clean = clean[:found[0]] + clean[found[1]:]
			altered = true
			matched = true
		}
	}

	if removed := strings.ReplaceAll(clean, selfClosingThinkTag, ""); removed != clean {
		clean = removed
		altered = true
	}
	for _, variant := range inlineThinkVariants {
		if removed := strings.ReplaceAll(clean, variant.close, ""); removed != clean {
			clean = removed
			altered = true
		}
	}

	// An unclosed opening tag (stream truncated mid-thought) makes the
	// remaining text a thought as well.
	if idx := unclosedThinkOpenIndex(clean); idx >= 0 {
		for _, variant := range inlineThinkVariants {
			if strings.HasPrefix(clean[idx:], variant.open) {
				appendThought(strings.TrimLeft(clean[idx+len(variant.open):], " \t\n"))
				break
			}
		}
		clean = clean[:idx]
		altered = true
	}

	// Only reshape the output when something was actually removed;
	// think-free content passes through byte-identical.
	if altered {
		clean = strings.TrimSpace(clean)
	}

	return clean, caught.String()
}

// unclosedThinkOpenIndex returns the earliest opening inline reasoning
// tag position still present in clean, or -1.
func unclosedThinkOpenIndex(clean string) int {
	best := -1
	for _, variant := range inlineThinkVariants {
		if idx := strings.Index(clean, variant.open); idx >= 0 && (best < 0 || idx < best) {
			best = idx
		}
	}
	return best
}

// joinThinkReasoning merges inline-captured thinking text into provider
// reasoning payloads. Blank inputs pass through untouched.
func joinThinkReasoning(existing, think string) string {
	if strings.TrimSpace(existing) == "" {
		return think
	}
	if strings.TrimSpace(think) == "" {
		return existing
	}
	return existing + "\n\n" + think
}

// applyInlineThinkSplit post-processes a parsed response: content that
// carries inline think blocks is cleaned and the captured text is folded
// into the reasoning payload.
func applyInlineThinkSplit(content, reasoning string) (string, string) {
	clean, think := SplitInlineThink(content)
	if think == "" {
		return content, reasoning
	}
	return clean, joinThinkReasoning(reasoning, think)
}

// cleanStreamingContent applies think-tag cleanup to an accumulated
// streaming snapshot so in-progress thought text is never displayed as
// chat output. While a thinking block is open the display snapshot
// shrinks to the stable prefix before it; when the closing tag arrives
// in a later event, the answer reappears in the snapshot.
func cleanStreamingContent(accumulated string) string {
	if !strings.Contains(accumulated, "<") {
		return accumulated
	}
	clean, _ := SplitInlineThink(accumulated)
	return trimPartialTagTail(clean)
}

// trimPartialTagTail removes a trailing fragment that could still grow
// into an opening inline reasoning tag (a cut-off "<", "<thi", ...) so
// the client never flashes a half-received tag mid-stream.
func trimPartialTagTail(s string) string {
	idx := strings.LastIndex(s, "<")
	if idx < 0 {
		return s
	}
	tail := s[idx:]
	if strings.Contains(tail, ">") {
		return s
	}
	for _, variant := range inlineThinkVariants {
		if strings.HasPrefix(variant.open, tail) {
			return s[:idx]
		}
	}
	return s
}
