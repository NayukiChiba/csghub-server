package mcpserver

import (
	"slices"
	"strings"

	"opencsg.com/csghub-server/builder/store/database"
)

// This file implements deterministic extraction of constraints the user typed
// verbatim. The philosophy matches applyDateConstraint: words the user
// actually wrote are hard constraints, and the search must not silently drop
// them just because the LLM forgot to repeat them in its structured output
// (observed: kimi drops "多模态" from keywords, secllm omits modality for
// 图像分类). LLM-inferred constraints stay soft.

// modalityCanonicalNames are the first elements of the modality synonym
// groups in querySynonymGroups — the canonical value the search filter
// expects for each modality.
var modalityCanonicalNames = map[string]bool{
	"image": true, "video": true, "audio": true,
	"text": true, "3d": true, "multimodal": true,
}

// languageCanonicalNames works the same way for the natural-language groups.
var languageCanonicalNames = map[string]bool{
	"chinese": true, "english": true, "japanese": true, "korean": true,
	"french": true, "german": true, "spanish": true, "russian": true,
	"multilingual": true,
}

// messageGroupIDs returns the set of synonym groups touched by the message's
// tokens, keyed by the group's canonical (first) term, plus the set of raw
// lowercased tokens for exact matching. Unspaced Chinese is segmented against
// the synonym keys first, so "图像分类" covers the image and classification
// groups.
func messageGroupIDs(message string) (groups map[string]bool, tokens map[string]bool) {
	groups = make(map[string]bool)
	tokens = make(map[string]bool)
	add := func(term string) {
		if term == "" || queryStopwords[term] {
			return
		}
		tokens[term] = true
		if g, ok := synonymIndex[term]; ok && len(g) > 0 {
			groups[g[0]] = true
		}
	}
	for _, token := range splitQueryTokens(strings.ToLower(message)) {
		if token == "" || queryStopwords[token] {
			continue
		}
		if segments := segmentCJK(token); segments != nil {
			for _, seg := range segments {
				add(seg)
			}
			continue
		}
		add(token)
	}
	return groups, tokens
}

// explicitModality returns the canonical modality the user named verbatim in
// the message (图像/图片 -> image, 语音 -> audio, 多模态 -> multimodal...), or
// "" when none appears.
func explicitModality(message string) string {
	groups, _ := messageGroupIDs(message)
	// stable order so "图像和视频" picks deterministically
	for _, name := range []string{"multimodal", "3d", "image", "video", "audio", "text"} {
		if groups[name] {
			return name
		}
	}
	return ""
}

// explicitLanguage returns the canonical content language the user named
// verbatim (中文 -> chinese, 英文 -> english...), or "" when none appears.
func explicitLanguage(message string) string {
	groups, _ := messageGroupIDs(message)
	for _, name := range []string{"chinese", "english", "japanese", "korean", "french", "german", "spanish", "russian", "multilingual"} {
		if groups[name] {
			return name
		}
	}
	return ""
}

// maxExplicitTags caps deterministically matched tags per query.
const maxExplicitTags = 3

// matchExplicitTags finds vocabulary tags the user explicitly referred to: a
// tag matches when every word of its name is covered by a message token or
// that token's synonym group — "图像分类" covers {image, classification} and
// therefore matches task/image-classification, while "文本分类" does not
// match task/token-classification (the "token" word is absent). Only task and
// license tags are considered; language is handled by the structured
// content-language filter and the remaining categories are too noisy for
// word-coverage matching.
func matchExplicitTags(message string, vocab []*database.Tag) []TagArg {
	groups, tokens := messageGroupIDs(message)
	if len(tokens) == 0 {
		return nil
	}
	covered := func(word string) bool {
		if tokens[word] {
			return true
		}
		if g, ok := synonymIndex[word]; ok && len(g) > 0 && groups[g[0]] {
			return true
		}
		// a word that only occurs inside a multi-word synonym key (e.g.
		// "choice" in the "multiple choice" group) is covered when the
		// message touched that group — "多选题" covers both words of the
		// multiple-choice tag
		for key, g := range synonymIndex {
			if len(g) == 0 || !groups[g[0]] || !strings.Contains(key, " ") {
				continue
			}
			if slices.Contains(strings.Split(key, " "), word) {
				return true
			}
		}
		return false
	}
	var matched []TagArg
	for _, t := range vocab {
		if t.Category != "task" && t.Category != "license" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(t.Name))
		words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == ' ' || r == '_' })
		if len(words) == 0 || len(words) > 3 {
			continue
		}
		allCovered := true
		for _, w := range words {
			if !covered(w) {
				allCovered = false
				break
			}
		}
		if allCovered {
			matched = append(matched, TagArg{Category: t.Category, Name: t.Name, Hard: true})
			if len(matched) >= maxExplicitTags {
				break
			}
		}
	}
	return matched
}

// applyExplicitConstraints pins the constraints the user typed verbatim into
// the parse result, overriding or completing the LLM's guesses. It runs on
// every path — including the LLM-failure fallback — so an unreliable model
// cannot drop a condition the user stated outright. Tags matched from the
// real vocabulary are marked Hard, which licenses a tag-only fallback search
// (they are the user's own words, not an LLM hallucination).
func applyExplicitConstraints(parsed *ParseDatasetQueryResult, message string, vocab []*database.Tag) {
	if m := explicitModality(message); m != "" {
		if !strings.EqualFold(parsed.Modality, m) {
			parsed.Modality = m
		}
		parsed.ModalityHard = true
	}
	if l := explicitLanguage(message); l != "" {
		parsed.Language = l
		parsed.LanguageHard = true
	}
	for _, hard := range matchExplicitTags(message, vocab) {
		found := false
		for i, t := range parsed.Tags {
			if strings.EqualFold(t.Category, hard.Category) && strings.EqualFold(t.Name, hard.Name) {
				parsed.Tags[i].Hard = true // the LLM guessed it AND the user said it
				found = true
				break
			}
		}
		if !found {
			parsed.Tags = append(parsed.Tags, hard)
		}
	}
	// A superlative question only makes sense against an ordering; without a
	// sort the LLM did not read it as a ranking request, so leave it a plain
	// search.
	if parsed.Sort != "" && explicitExtremum(message) {
		parsed.Extremum = true
	}
}

// extremumPronouns turn a sorted listing into a "which one is the most X"
// question: the user wants the top entry (plus ties), not the full ranking.
var extremumPronouns = []string{"哪个", "哪一", "哪几", "谁", "哪家", "什么"}

// extremumSuperlatives are the english superlative markers ("most downloaded",
// "largest"); the chinese marker 最 is checked separately.
var extremumSuperlatives = []string{
	"most", "largest", "biggest", "smallest", "highest", "lowest",
	"earliest", "latest", "newest", "oldest",
}

// explicitExtremum reports whether the message asks a superlative question
// ("下载最多的是哪个", "which dataset is the largest") rather than for a
// sorted list ("下载最多的数据集有哪些"). Detection is deterministic: a
// question pronoun plus a superlative marker.
func explicitExtremum(message string) bool {
	msg := strings.ToLower(message)
	superlative := strings.Contains(msg, "最")
	for i := 0; !superlative && i < len(extremumSuperlatives); i++ {
		superlative = strings.Contains(msg, extremumSuperlatives[i])
	}
	if !superlative {
		return false
	}
	for _, p := range extremumPronouns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return strings.Contains(msg, "which") || strings.Contains(msg, "what")
}

// allTagsHard reports whether every tag filter is user-stated. A tag-only
// fallback is legitimate only then: dropping keywords in favour of an
// LLM-guessed tag answered "多模态评估" with unrelated benchmarks.
func allTagsHard(tags []TagArg) bool {
	if len(tags) == 0 {
		return false
	}
	return slices.ContainsFunc(tags, func(t TagArg) bool { return t.Hard }) &&
		slices.IndexFunc(tags, func(t TagArg) bool { return !t.Hard }) == -1
}
