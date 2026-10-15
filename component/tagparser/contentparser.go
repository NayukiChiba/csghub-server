package tagparser

import (
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MetaTags parse metadata of README file, return tags found
func MetaTags(readme string) (map[string][]string, error) {
	meta := metaText(readme)
	if len(meta) == 0 {
		return map[string][]string{}, nil
	}

	categoryContents := make(map[string]any)
	//parse yaml string
	err := yaml.Unmarshal([]byte(meta), categoryContents)
	if err != nil {
		slog.Error("error unmarshall meta for tags", slog.Any("error", err), slog.String("meta", meta))
		return nil, err
	}
	categoryTags := make(map[string][]string)
	for category, content := range categoryContents {
		category = formatCategoryName(category)
		slog.Debug("tag category map content", slog.String("category", category), slog.Any("content", content))
		//TODO: define different content parser
		if tagName, match := content.(string); match {
			categoryTags[category] = append(categoryTags[category], strings.TrimSpace(tagName))
		} else if tagNames, match := content.([]interface{}); match {
			for _, tagNameValue := range tagNames {
				if tagName, isString := tagNameValue.(string); isString {
					categoryTags[category] = append(categoryTags[category], strings.TrimSpace(tagName))
				} else {
					//ignore
					slog.Warn("ignore unknown tag format", slog.Any("tagNameValue", tagNameValue))
				}
			}
		} else {
			slog.Error("Unknown meta format", slog.Any("content", content))
			//alow continue parsing
			// return nil, errors.New("unknown meta format")
		}
	}

	return uniqueTags(categoryTags), nil
}

func uniqueTags(categoryTags map[string][]string) map[string][]string {
	uniqueTags := make(map[string][]string)
	for c, ts := range categoryTags {
		//must sort before slice compact
		slices.Sort(ts)
		uniqueTags[c] = slices.Compact(ts)
	}
	return uniqueTags
}

func metaText(readme string) string {
	splits := strings.Split(readme, "---")
	if len(splits) < 2 {
		return ""
	}

	return splits[1]
}

const maxExtractedDescriptionRunes = 500

// ExtractDescription derives a short plain-text description from a README: the
// frontmatter is stripped (same "---" convention as metaText), then the first
// paragraph that is not a heading, image, HTML block or table row is returned
// with whitespace collapsed, truncated to maxExtractedDescriptionRunes runes.
// Returns "" when the README has no usable prose.
func ExtractDescription(readme string) string {
	body := readme
	if splits := strings.Split(readme, "---"); len(splits) >= 3 {
		body = strings.Join(splits[2:], "---")
	}

	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if b.Len() > 0 {
				break // first prose paragraph ends at the blank line
			}
			continue
		}
		if b.Len() == 0 {
			switch line[0] {
			case '#', '!', '<', '|':
				continue
			}
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
	}

	runes := []rune(b.String())
	if len(runes) > maxExtractedDescriptionRunes {
		return string(runes[:maxExtractedDescriptionRunes])
	}
	return string(runes)
}
