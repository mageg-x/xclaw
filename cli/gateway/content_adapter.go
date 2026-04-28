package gateway

import (
	"fmt"
	"regexp"
	"strings"
)

type ContentAdapter struct {
	MaxTextLen   int
	MaxButtonLen int
}

func NewContentAdapter() *ContentAdapter {
	return &ContentAdapter{
		MaxTextLen:   16000,
		MaxButtonLen: 200,
	}
}

func (a *ContentAdapter) Adapt(text string, actions []Action, cap CapabilityProfile) (string, []Action, []ContentFragment) {
	if cap.MaxTextLen > 0 {
		a.MaxTextLen = cap.MaxTextLen
	}

	fragments := a.Fragment(text)

	adaptedText := fragments[0].Content
	if len(fragments) > 1 {
		adaptedText = fragments[0].Content + fmt.Sprintf("\n\n[共 %d 片段，当前第 1 片段]", len(fragments))
	}

	adaptedActions := a.adaptActions(actions, cap)

	return adaptedText, adaptedActions, fragments
}

type ContentFragment struct {
	Index   int    `json:"index"`
	Content string `json:"content"`
	Total   int    `json:"total"`
}

func (a *ContentAdapter) Fragment(text string) []ContentFragment {
	text = strings.TrimSpace(text)
	if text == "" {
		return []ContentFragment{{Index: 0, Content: "", Total: 1}}
	}
	if len(text) <= a.MaxTextLen {
		return []ContentFragment{{Index: 0, Content: text, Total: 1}}
	}

	fragments := splitBySections(text, a.MaxTextLen)
	if len(fragments) <= 1 {
		fragments = splitByParagraphs(text, a.MaxTextLen)
	}
	if len(fragments) <= 1 {
		fragments = splitByLines(text, a.MaxTextLen)
	}
	if len(fragments) <= 1 {
		fragments = splitHard(text, a.MaxTextLen)
	}

	total := len(fragments)
	out := make([]ContentFragment, total)
	for i, f := range fragments {
		out[i] = ContentFragment{Index: i, Content: f, Total: total}
	}
	return out
}

func (a *ContentAdapter) adaptActions(actions []Action, cap CapabilityProfile) []Action {
	if !cap.SupportsButtons || len(actions) == 0 {
		return nil
	}

	var adapted []Action
	for _, act := range actions {
		label := act.Label
		if len(label) > a.MaxButtonLen {
			label = label[:a.MaxButtonLen-3] + "..."
		}
		adapted = append(adapted, Action{
			Label: label,
			Value: act.Value,
			URL:   act.URL,
		})
	}
	return adapted
}

func splitBySections(text string, maxLen int) []string {
	sectionRe := regexp.MustCompile(`(?m)^(#{1,6}\s)`)
	indices := sectionRe.FindAllStringIndex(text, -1)
	if len(indices) <= 1 {
		return nil
	}

	var fragments []string
	start := 0
	for i := 1; i < len(indices); i++ {
		sectionEnd := indices[i][0]
		section := text[start:sectionEnd]
		if len(section) > maxLen {
			return nil
		}
		if start > 0 && len(fragments) > 0 && len(fragments[len(fragments)-1])+len(section) <= maxLen {
			fragments[len(fragments)-1] += section
		} else {
			fragments = append(fragments, section)
		}
		start = sectionEnd
	}
	if start < len(text) {
		remaining := text[start:]
		if len(fragments) > 0 && len(fragments[len(fragments)-1])+len(remaining) <= maxLen {
			fragments[len(fragments)-1] += remaining
		} else if len(remaining) <= maxLen {
			fragments = append(fragments, remaining)
		} else {
			return nil
		}
	}
	if len(fragments) <= 1 {
		return nil
	}
	return fragments
}

func splitByParagraphs(text string, maxLen int) []string {
	paragraphs := strings.Split(text, "\n\n")
	var fragments []string
	var current strings.Builder

	for _, p := range paragraphs {
		if current.Len() > 0 && current.Len()+2+len(p) > maxLen {
			fragments = append(fragments, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(p)
	}
	if current.Len() > 0 {
		fragments = append(fragments, current.String())
	}
	if len(fragments) <= 1 {
		return nil
	}
	return fragments
}

func splitByLines(text string, maxLen int) []string {
	lines := strings.Split(text, "\n")
	var fragments []string
	var current strings.Builder

	for _, line := range lines {
		if current.Len() > 0 && current.Len()+1+len(line) > maxLen {
			fragments = append(fragments, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		fragments = append(fragments, current.String())
	}
	if len(fragments) <= 1 {
		return nil
	}
	return fragments
}

func splitHard(text string, maxLen int) []string {
	var fragments []string
	for i := 0; i < len(text); i += maxLen {
		end := i + maxLen
		if end > len(text) {
			end = len(text)
		}
		fragments = append(fragments, text[i:end])
	}
	return fragments
}
