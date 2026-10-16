package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"opencsg.com/csghub-server/common/types"
)

// ClarifyScopeQuestion builds a clarification question for an ambiguous
// zero-result follow-up, or nil when the zero result is trustworthy and
// should be reported as-is.
//
// The ambiguity pattern (observed in real sessions): a pure follow-up like
// "这里面哪个数据规模最大？" carries no topic of its own, the LLM folds the
// whole history and resurrects constraints from superseded topics (plus the
// occasional hallucinated tag), and the AND search honestly finds nothing.
// Instead of reporting "the combination does not exist" — which blames the
// user's scope for the parser's confusion — the chat asks which scope was
// meant. Each option's Send text rephrases the original question inside that
// scope as a self-contained message, so the next turn parses deterministically
// (explicit tags, extremum and size/date extraction all see the scope words
// verbatim) with no server-side dialogue state.
//
// The trigger is deliberately narrow; a clarification prompt is only better
// than an honest zero when ALL of these hold:
//   - the user stated no hard constraint verbatim in the current message
//     (hard modality/language/tags, or a date/size bound) — an explicit
//     constraint makes the zero result trustworthy;
//   - the current message has no topic terms of its own (a topical message
//     defines its scope; nothing is ambiguous);
//   - an earlier user turn provides a topic to clarify against.
func ClarifyScopeQuestion(history []string, message string, parsed *ParseDatasetQueryResult) *types.AIChatQuestion {
	if parsed == nil {
		return nil
	}
	if parsed.ModalityHard || parsed.LanguageHard {
		return nil
	}
	for _, t := range parsed.Tags {
		if t.Hard {
			return nil
		}
	}
	if extractDateConstraint(message, time.Now()) != nil || extractSizeConstraint(message) != nil {
		return nil
	}
	if hasNonLanguageTopicTerms(message) {
		return nil
	}
	topic := lastTopicTerms(history)
	if topic == "" {
		return nil
	}
	compact := strings.ReplaceAll(topic, " ", "")
	return &types.AIChatQuestion{
		Question: "没有找到符合条件的数据集。这次问题里没有新的条件，可能是范围理解偏了，确认一下：",
		Options: []types.AIChatQuestionOption{
			{
				Label: fmt.Sprintf("在刚才的范围内找（%s）", compact),
				Send:  fmt.Sprintf("%s数据集中，%s", compact, message),
			},
			{
				Label: "不限范围，在全平台找",
				Send:  fmt.Sprintf("所有数据集中，%s", message),
			},
		},
	}
}

// lastTopicTerms returns the recognized topic terms of the most recent
// topical user message, newest first ("" when none). Follow-up phrasings
// without their own topic are skipped.
func lastTopicTerms(history []string) string {
	for i := len(history) - 1; i >= 0; i-- {
		if terms := recognizedTopicTerms(history[i]); terms != "" {
			return terms
		}
	}
	return ""
}
