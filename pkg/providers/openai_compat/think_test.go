package openai_compat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tagFor builds inline thinking tags by concatenation (instead of tag
// literals) so the open and close halves can never drift apart in the
// test source either.
func tagFor(kind string) (open string, close string) {
	return "<" + kind + ">", "</" + kind + ">"
}

func TestCloseOf_DerivesMirroredCloseTag(t *testing.T) {
	for _, kind := range []string{"think", "thought", "reasoning"} {
		open, wantClose := tagFor(kind)
		if got := closeOf(open); got != wantClose {
			t.Fatalf("closeOf(%q) = %q, want %q", open, got, wantClose)
		}
	}
	if got := closeOf("not-a-tag"); got != "" {
		t.Fatalf("closeOf(non-tag) = %q, want empty", got)
	}
}

func TestSplitInlineThink_StripsCanonicalThinkBlock(t *testing.T) {
	open, close := tagFor("think")
	content := open + "\ncalculation notes\n" + close + "\n\nFinal answer here."
	clean, think := SplitInlineThink(content)
	if clean != "Final answer here." {
		t.Fatalf("clean = %q, want %q", clean, "Final answer here.")
	}
	if think != "calculation notes" {
		t.Fatalf("think = %q, want %q", think, "calculation notes")
	}
}

func TestSplitInlineThink_NoThinkContentPassthrough(t *testing.T) {
	content := "Plain reply with <div> markup and no tags at all."
	clean, think := SplitInlineThink(content)
	if clean != content || think != "" {
		t.Fatalf("passthrough broken: clean=%q think=%q", clean, think)
	}
}

func TestSplitInlineThink_VariantTags(t *testing.T) {
	thoughtOpen, thoughtClose := tagFor("thought")
	reasoningOpen, reasoningClose := tagFor("reasoning")
	content := thoughtOpen + "glm steps" + thoughtClose + " A " + reasoningOpen + "relay steps" + reasoningClose + " B"
	clean, think := SplitInlineThink(content)
	if clean != "A  B" {
		t.Fatalf("clean = %q, want %q", clean, "A  B")
	}
	want := "glm steps\n\nrelay steps"
	if think != want {
		t.Fatalf("think = %q, want %q", think, want)
	}
}

func TestSplitInlineThink_UnclosedOpenTreatedAsThought(t *testing.T) {
	open, _ := tagFor("think")
	content := "Intro line still visible. " + open + " everything after the tag is thought"
	clean, think := SplitInlineThink(content)
	if clean != "Intro line still visible." {
		t.Fatalf("clean = %q, want intro prefix", clean)
	}
	if think != "everything after the tag is thought" {
		t.Fatalf("think = %q", think)
	}
}

func TestSplitInlineThink_SelfClosingAndStrayClose(t *testing.T) {
	_, close := tagFor("think")
	content := "<think/> Answer with stray " + close + " tag removed."
	clean, think := SplitInlineThink(content)
	if clean != "Answer with stray  tag removed." {
		t.Fatalf("clean = %q", clean)
	}
	if think != "" {
		t.Fatalf("think = %q, want empty", think)
	}
}

func TestSplitInlineThink_MultipleCanonicalBlocks(t *testing.T) {
	open, close := tagFor("think")
	content := open + "first" + close + " mid " + open + " second " + close + " done"
	clean, think := SplitInlineThink(content)
	if clean != "mid  done" {
		t.Fatalf("clean = %q", clean)
	}
	if think != "first\n\nsecond" {
		t.Fatalf("think = %q, want %q", think, "first\n\nsecond")
	}
}

func TestJoinThinkReasoning_MergesSources(t *testing.T) {
	if got := joinThinkReasoning("", "inline"); got != "inline" {
		t.Fatalf("empty existing -> got %q", got)
	}
	if got := joinThinkReasoning("structured", ""); got != "structured" {
		t.Fatalf("empty think -> got %q", got)
	}
	got := joinThinkReasoning("structured", "inline")
	if got != "structured\n\ninline" {
		t.Fatalf("merge -> got %q", got)
	}
}

func TestCleanStreamingContent_HidesOpenThought(t *testing.T) {
	open, close := tagFor("think")
	cases := []struct {
		acc  string
		want string
	}{
		{"没有任何标签", "没有任何标签"},
		{"Hello " + open, "Hello"},
		{"Hello " + open + " secret thoughts", "Hello"},
		{"Hello " + open + " thoughts" + close + " answer", "Hello  answer"},
		// Partial open tag held back until it completes.
		{"Hello <thin", "Hello "},
		{"Hello " + close, "Hello"},
		// A partial "<" that cannot grow into a thinking tag stays put.
		{"Click <b for bold", "Click <b for bold"},
	}
	for _, tc := range cases {
		if got := cleanStreamingContent(tc.acc); got != tc.want {
			t.Fatalf("cleanStreamingContent(%q) = %q, want %q", tc.acc, got, tc.want)
		}
	}
}

func TestProviderChat_InlineThinkStripped(t *testing.T) {
	open, close := tagFor("think")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content":          open + "silent math" + close + "42",
						"reasoning_content": "structured thoughts",
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewProvider("key", server.URL, "")
	out, err := p.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}, nil, "deepseek-r1", nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if out.Content != "42" {
		t.Fatalf("Content = %q, want %q", out.Content, "42")
	}
	if got := out.ReasoningContent; got != "structured thoughts\n\nsilent math" {
		t.Fatalf("ReasoningContent = %q", got)
	}
}

func TestProviderChatStreamEvents_InlineThinkStrippedAndHidden(t *testing.T) {
	// open kept via tagFor below; only close is needed directly here.
	_, close := tagFor("think")
	// Deltas deliberately split the opening and closing tags so the
	// snapshot filter has to hold back partial tags.
	deltas := []string{"The answer is", " <thi", "nk>hidden reasoning", close, "42 plus"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range deltas {
			payload := map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]any{"content": delta}},
				},
			}
			encoded, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var shown []string
	p := NewProvider("key", server.URL, "")
	out, err := p.ChatStreamEvents(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"deepseek-r1",
		nil,
		func(chunk StreamChunk) {
			if chunk.Content != "" {
				shown = append(shown, chunk.Content)
			}
		},
	)
	if err != nil {
		t.Fatalf("ChatStreamEvents() error = %v", err)
	}

	// Final content carries the answer, the inline block is gone.
	if out.Content != "The answer is 42 plus" {
		t.Fatalf("Content = %q", out.Content)
	}
	if out.ReasoningContent != "hidden reasoning" {
		t.Fatalf("ReasoningContent = %q", out.ReasoningContent)
	}
	// No streamed snapshot ever exposes thinking text or raw tags.
	for _, snapshot := range shown {
		if strings.Contains(snapshot, "hidden") || strings.Contains(snapshot, "thi") && strings.Contains(snapshot, "<") {
			t.Fatalf("snapshot leaked reasoning: %q", snapshot)
		}
	}
	if shown[len(shown)-1] != "The answer is 42 plus" {
		t.Fatalf("last snapshot = %q, want final text", shown[len(shown)-1])
	}
}
