package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Rendering for the search-everywhere modal (the `/` op). A text field on top;
// below it either the recent-search list (empty input) or the live, ranked
// results (typed input), with the focused row marked. Styling reuses view.go's
// palette. A small freshness-style hint conveys whether pods have joined the
// index yet.

// searchKindStyle dims the small "[ns]/[deploy]/[pod]" kind tag on a result row.
var searchKindStyle = lipgloss.NewStyle().Faint(true)

// searchResultsCap bounds how many result rows render at once, so a 1-char query
// matching everything stays a tidy modal rather than a wall of rows. The cursor
// is clamped to the result count (not the cap), so navigation never desyncs.
const searchResultsCap = 12

// renderSearchModal renders the search frame: the title, the input field, a hint
// line, then recents (empty input) or ranked results (typed input).
func (m Model) renderSearchModal() string {
	ss := m.searchModal
	var b strings.Builder
	b.WriteString(titleStyle.Render("kc") + "  " + titleStyle.Render("search") + "\n\n")

	// The text input field at the top.
	b.WriteString(ss.input.View() + "\n")

	// Index-freshness hint: pods join the index asynchronously, so say whether
	// they have ("updated") or are still in flight ("indexing pods…").
	idx := hintStyle.Render("indexing pods…")
	if ss.podsLoaded {
		idx = hintStyle.Render(fmt.Sprintf("index updated · %d namespaces · %d deployments · %d pods",
			len(ss.nsItems), len(ss.depItems), len(ss.podItems)))
	}
	b.WriteString(idx + "\n\n")

	if m.searchShowingRecents(ss) {
		b.WriteString(m.renderSearchRecents(ss))
	} else {
		b.WriteString(m.renderSearchResults(ss))
	}

	b.WriteString("\n\n" + footerStyle.Render("↑/↓ move · enter jump · esc close"))
	return b.String()
}

// renderSearchRecents renders the most-recent past searches (empty-input state),
// the focused row marked. Enter on a focused recent re-runs it.
func (m Model) renderSearchRecents(ss *searchState) string {
	var b strings.Builder
	if len(ss.recents) == 0 {
		b.WriteString(hintStyle.Render("  type to search namespaces, deployments, pods"))
		return b.String()
	}
	b.WriteString(headerStyle.Render("recent searches") + "\n")
	for i, q := range ss.recents {
		marker := "  "
		line := q
		if i == ss.cursor {
			marker = "› "
			line = selectedStyle.Render(q)
		}
		b.WriteString(marker + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSearchResults renders the live, ranked results (typed-input state). Each
// row shows a small kind tag + the resource path (namespace · deploy · pod). The
// focused row is marked; the list is capped (searchResultsCap) with a "… N more"
// hint when there is more.
func (m Model) renderSearchResults(ss *searchState) string {
	var b strings.Builder
	if len(ss.results) == 0 {
		b.WriteString(hintStyle.Render("  no matches"))
		return b.String()
	}

	end := len(ss.results)
	if end > searchResultsCap {
		end = searchResultsCap
	}
	for i := 0; i < end; i++ {
		it := ss.results[i]
		tag := searchKindStyle.Render(fmt.Sprintf("[%-6s]", searchKindTag(it.kind)))
		path := searchPath(it)
		marker := "  "
		if i == ss.cursor {
			marker = "› "
			path = selectedStyle.Render(path)
		}
		b.WriteString(marker + tag + " " + path + "\n")
	}
	if len(ss.results) > end {
		b.WriteString(hintStyle.Render(fmt.Sprintf("  … %d more", len(ss.results)-end)))
	}
	return strings.TrimRight(b.String(), "\n")
}
