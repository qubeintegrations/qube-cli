package ops

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The descriptions in the v2 spec are Markdown. Help renders the subset the API writes for a
// terminal: paragraphs, `#` headings, `- `/`* `/`1. ` list items, `> ` quotes and code
// blocks; inline `code`, [links](url), **bold**, *emphasis* and backslash escapes. Anything
// else passes through as text.

type blockKind int

const (
	paraBlock blockKind = iota
	headingBlock
	itemBlock
	quoteBlock
	codeBlock
)

// mdBlock is one block of a Markdown text.
type mdBlock struct {
	kind   blockKind
	marker string // a list item's marker: "-" or "1."
	level  int    // a list item's nesting depth
	text   string // inline Markdown, its lines joined by spaces (a code block's: verbatim)
	src    string // the block's own lines, as written
	gap    bool   // a blank line comes before it
}

var (
	headingLine = regexp.MustCompile(`^#{1,6}\s+(.*?)(\s+#+)?\s*$`)
	itemLine    = regexp.MustCompile(`^([-*+]|\d{1,9}[.)])\s+(.*)$`)
)

// parseBlocks splits Markdown into its blocks.
func parseBlocks(md string) []mdBlock {
	var out []mdBlock
	open := false // the last block can take a continuation line
	gap := false
	fence := ""
	for _, line := range strings.Split(strings.Replace(md, "\r\n", "\n", -1), "\n") {
		if fence != "" {
			last := &out[len(out)-1]
			last.src += "\n" + line
			switch {
			case strings.HasPrefix(strings.TrimSpace(line), fence):
				fence = ""
			case last.text == "":
				last.text = line
			default:
				last.text += "\n" + line
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			open, gap = false, true
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		add := func(b mdBlock) {
			b.gap, b.src = gap && len(out) > 0, line
			out = append(out, b)
			gap = false
		}
		appendLine := func(text string) {
			last := &out[len(out)-1]
			last.text += " " + text
			last.src += "\n" + line
		}
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence = trimmed[:3]
			add(mdBlock{kind: codeBlock})
			open = false
			continue
		case indent >= 4 && !open && (len(out) == 0 || out[len(out)-1].kind != itemBlock || !gap):
			if len(out) > 0 && out[len(out)-1].kind == codeBlock && !gap {
				last := &out[len(out)-1]
				last.text += "\n" + line[4:]
				last.src += "\n" + line
			} else {
				add(mdBlock{kind: codeBlock, text: strings.TrimLeft(line, " \t")})
			}
			continue
		case indent < 4 && headingLine.MatchString(trimmed):
			add(mdBlock{kind: headingBlock, text: headingLine.FindStringSubmatch(trimmed)[1]})
			open = false
			continue
		case itemLine.MatchString(trimmed):
			m := itemLine.FindStringSubmatch(trimmed)
			marker := m[1]
			if marker == "*" || marker == "+" {
				marker = "-"
			}
			add(mdBlock{kind: itemBlock, marker: marker, level: indent / 2, text: m[2]})
		case strings.HasPrefix(trimmed, ">"):
			text := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			if open && out[len(out)-1].kind == quoteBlock {
				appendLine(text)
			} else {
				add(mdBlock{kind: quoteBlock, text: text})
			}
		case open || (gap && len(out) > 0 && out[len(out)-1].kind == itemBlock && indent >= 2):
			// a lazy continuation line, or an item's indented second paragraph
			if gap {
				out[len(out)-1].src += "\n"
				gap = false
			}
			appendLine(trimmed)
		default:
			add(mdBlock{kind: paraBlock, text: trimmed})
		}
		open = true
	}
	return out
}

// renderMarkdown renders Markdown for a terminal, wrapped to width.
func renderMarkdown(md string, width int) string {
	var lines []string
	blocks := parseBlocks(md)
	for i, b := range blocks {
		if i > 0 && (b.gap || b.kind == headingBlock || blocks[i-1].kind == headingBlock) {
			lines = append(lines, "")
		}
		lines = append(lines, renderBlock(b, width)...)
	}
	return strings.Join(lines, "\n")
}

func renderBlock(b mdBlock, width int) []string {
	switch b.kind {
	case itemBlock:
		indent := strings.Repeat("  ", b.level)
		return wrapText(renderInline(b.text), width, indent+b.marker+" ", indent+strings.Repeat(" ", len(b.marker)+1))
	case quoteBlock:
		return wrapText(renderInline(b.text), width, "    ", "    ")
	case codeBlock:
		var out []string
		for _, l := range strings.Split(b.text, "\n") {
			out = append(out, strings.TrimRight("    "+l, " "))
		}
		return out
	}
	return wrapText(renderInline(b.text), width, "", "")
}

// wrapText fills words into lines of at most width runes, the first line after `first`
// and the rest after `rest`. A word is never broken, so a URL longer than a line stands on
// a line of its own.
func wrapText(text string, width int, first, rest string) []string {
	var out []string
	line, prefix := "", first
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = prefix + word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width:
			out = append(out, line)
			prefix = rest
			line = prefix + word
		default:
			line += " " + word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// renderInline turns inline Markdown into plain text: `code` is its content, a link is
// "text (url)" (or the url alone when that is its text), emphasis loses its markers and a
// backslash escape is the character it escapes.
func renderInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) && isPunct(s[i+1]) {
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
		case '`':
			if content, end, ok := codeSpan(s, i); ok {
				b.WriteString(content)
				i = end
				continue
			}
		case '[':
			if text, url, end, ok := link(s, i); ok {
				text = renderInline(text)
				if text == "" || text == url {
					b.WriteString(url)
				} else {
					b.WriteString(text + " (" + url + ")")
				}
				i = end
				continue
			}
		case '<':
			if end := strings.IndexByte(s[i:], '>'); end > 0 {
				inner := s[i+1 : i+end]
				if (strings.HasPrefix(inner, "http://") || strings.HasPrefix(inner, "https://")) && !strings.ContainsAny(inner, " <") {
					b.WriteString(inner)
					i += end + 1
					continue
				}
			}
		case '*':
			if inner, end, ok := emphasis(s, i); ok {
				b.WriteString(renderInline(inner))
				i = end
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

// codeSpan reads the `code` span opening at s[i]: its content and where it ends.
func codeSpan(s string, i int) (string, int, bool) {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	ticks := strings.Repeat("`", n)
	for j := i + n; j < len(s); {
		k := strings.Index(s[j:], ticks)
		if k < 0 {
			return "", 0, false
		}
		k += j
		if k+n < len(s) && s[k+n] == '`' { // a longer run: not the closing one
			for k < len(s) && s[k] == '`' {
				k++
			}
			j = k
			continue
		}
		content := s[i+n : k]
		if len(content) > 1 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.Trim(content, " ") != "" {
			content = content[1 : len(content)-1]
		}
		return content, k + n, true
	}
	return "", 0, false
}

// link reads the [text](url) opening at s[i]: its text, url and where it ends.
func link(s string, i int) (text, url string, end int, ok bool) {
	depth := 0
	j := i
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '`':
			if _, e, ok := codeSpan(s, j); ok {
				j = e - 1
			}
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	text = s[i+1 : j]
	depth = 0
	k := j + 1
	for ; k < len(s); k++ {
		if s[k] == '(' {
			depth++
		} else if s[k] == ')' {
			depth--
			if depth == 0 {
				break
			}
		} else if s[k] == ' ' && depth == 1 && !strings.HasPrefix(strings.TrimSpace(s[k:]), "\"") {
			return "", "", 0, false
		}
	}
	if k >= len(s) {
		return "", "", 0, false
	}
	dest := strings.TrimSpace(s[j+2 : k])
	if sp := strings.IndexByte(dest, ' '); sp >= 0 { // [text](url "title")
		dest = dest[:sp]
	}
	dest = strings.TrimSuffix(strings.TrimPrefix(dest, "<"), ">")
	if dest == "" {
		return "", "", 0, false
	}
	return text, dest, k + 1, true
}

// emphasis reads the *em* or **strong** span opening at s[i]: an opening run followed by
// a non-space, closed by the same run after a non-space. A lone or spaced `*` (as in
// "Quantity * Rate") is text.
func emphasis(s string, i int) (string, int, bool) {
	n := 1
	if i+1 < len(s) && s[i+1] == '*' {
		n = 2
	}
	run := s[i : i+n]
	if i+n >= len(s) || s[i+n] == ' ' || s[i+n] == '*' {
		return "", 0, false
	}
	if i > 0 && isWordByte(s[i-1]) {
		return "", 0, false
	}
	for j := i + n; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case s[j] == '`':
			if _, e, ok := codeSpan(s, j); ok {
				j = e - 1
			}
		case strings.HasPrefix(s[j:], run) && s[j-1] != ' ' && (j+n >= len(s) || s[j+n] != '*'):
			if j+n < len(s) && isWordByte(s[j+n]) {
				continue
			}
			return s[i+n : j], j + n, true
		}
	}
	return "", 0, false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// sentences splits inline Markdown into its sentences: each ends at ".", "!" or "?" (and
// any closing quote or bracket) before a space. Code spans and links are never split, and
// "e.g." and "i.e." end no sentence.
func sentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
			continue
		case '`':
			if _, end, ok := codeSpan(s, i); ok {
				i = end - 1
			}
			continue
		case '[':
			if _, _, end, ok := link(s, i); ok {
				i = end - 1
			}
			continue
		case '.', '!', '?':
		default:
			continue
		}
		j := i + 1
		for j < len(s) && strings.IndexByte(`)"'`, s[j]) >= 0 {
			j++
		}
		for _, q := range []string{"”", "’"} {
			if strings.HasPrefix(s[j:], q) {
				j += len(q)
			}
		}
		if j < len(s) && s[j] != ' ' {
			continue
		}
		word := s[strings.LastIndexByte(s[:i+1], ' ')+1 : i+1]
		if word == "e.g." || word == "i.e." || word == "(e.g." || word == "(i.e." {
			continue
		}
		if sentence := strings.TrimSpace(s[start:j]); sentence != "" {
			out = append(out, sentence)
		}
		start = j
		i = j - 1
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// brief keeps a Markdown text's whole blocks up to about max characters, and as much of
// the next paragraph, item or quote as fits in whole sentences. It returns Markdown, and
// whether it left anything out. It cuts only between blocks or sentences, so never inside
// a link, and a heading whose section it cuts off goes too.
func brief(md string, max int) (string, bool) {
	blocks := parseBlocks(md)
	var b strings.Builder
	kept := 0
	for i, bl := range blocks {
		sep := ""
		if i > 0 {
			sep = "\n"
			if bl.gap {
				sep = "\n\n"
			}
		}
		if b.Len()+len(sep)+len(bl.src) <= max {
			b.WriteString(sep + bl.src)
			kept = i + 1
			continue
		}
		room := max - b.Len() - len(sep)
		if bl.kind != headingBlock && bl.kind != codeBlock && room >= 150 {
			prefix := ""
			switch bl.kind {
			case itemBlock:
				prefix = strings.Repeat("  ", bl.level) + bl.marker + " "
			case quoteBlock:
				prefix = "> "
			}
			part := prefix
			for _, s := range sentences(bl.text) {
				if len(part)+len(s)+1 > room {
					break
				}
				if part != prefix {
					part += " "
				}
				part += s
			}
			if part != prefix {
				b.WriteString(sep + part + " …")
				return b.String(), true
			}
		}
		if kept > 0 && blocks[kept-1].kind == headingBlock {
			return briefUpTo(blocks, kept-1), true
		}
		if b.Len() == 0 { // not even the first sentence fits: as much of it as does
			if s := sentences(bl.text); len(s) > 0 {
				return clipMarkdown(s[0], max), true
			}
		}
		return b.String(), true
	}
	return md, false
}

// briefUpTo rejoins the first n blocks as written.
func briefUpTo(blocks []mdBlock, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			if blocks[i].gap {
				b.WriteString("\n\n")
			} else {
				b.WriteString("\n")
			}
		}
		b.WriteString(blocks[i].src)
	}
	return b.String()
}

// clipMarkdown shortens inline Markdown to at most max bytes at a space outside any code
// span or link, ending it with "…".
func clipMarkdown(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := 0
	for i := 0; i < len(s) && i < max; i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			if _, end, ok := codeSpan(s, i); ok {
				i = end - 1
			}
		case '[':
			if _, _, end, ok := link(s, i); ok {
				i = end - 1
			}
		case ' ':
			cut = i
		}
	}
	if cut == 0 {
		return ""
	}
	return strings.TrimRight(s[:cut], " ,;:") + " …"
}

// clip shortens text to at most max runes at a word boundary, ending it with "…".
func clip(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	r := []rune(text)
	cut := string(r[:max-1])
	if sp := strings.LastIndexByte(cut, ' '); sp > 0 {
		cut = cut[:sp]
	}
	return strings.TrimRight(cut, " ,;:") + " …"
}
