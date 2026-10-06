package agents

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// A small stdlib-only HTML to markdown-ish text converter (golang.org/x/net/html
// is not a dependency of the hub). It is a forgiving tokenizer, not a full
// parser: enough to pull readable text out of ordinary pages.

var (
	htmlSkipTags  = map[string]bool{"script": true, "style": true, "noscript": true, "nav": true, "footer": true, "aside": true, "svg": true, "template": true, "iframe": true, "select": true, "button": true, "title": true}
	htmlBlockTags = map[string]bool{"p": true, "div": true, "section": true, "article": true, "main": true, "header": true, "ul": true, "ol": true, "table": true, "tr": true, "blockquote": true, "figure": true, "figcaption": true, "dl": true, "dt": true, "dd": true, "hr": true, "details": true, "summary": true, "address": true, "fieldset": true}
	htmlAttrRe    = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	htmlBlankRe   = regexp.MustCompile(`\n{3,}`)
)

type htmlWriter struct {
	b      strings.Builder
	inPre  bool
	links  []linkMark
	listOL []int // per open list: 0 for ul, else the next ol number
}

type linkMark struct {
	start int
	href  string
}

func (w *htmlWriter) atLineStart() bool {
	s := w.b.String()
	return s == "" || s[len(s)-1] == '\n'
}

func (w *htmlWriter) newline(n int) {
	s := w.b.String()
	if s == "" {
		return
	}
	have := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n' && have < n; i-- {
		have++
	}
	for ; have < n; have++ {
		w.b.WriteByte('\n')
	}
}

func (w *htmlWriter) text(s string) {
	if w.inPre {
		w.b.WriteString(s)
		return
	}
	prevSpace := w.atLineStart() || strings.HasSuffix(w.b.String(), " ")
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '\f' || r == 0xa0 {
			if !prevSpace {
				w.b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		w.b.WriteRune(r)
		prevSpace = false
	}
}

func tagAttrs(s string) map[string]string {
	m := map[string]string{}
	for _, a := range htmlAttrRe.FindAllStringSubmatch(s, -1) {
		v := a[2] + a[3] + a[4]
		m[strings.ToLower(a[1])] = html.UnescapeString(v)
	}
	return m
}

// skipElement returns the index just past the close tag matching the opening
// tag `name` whose content starts at from, counting nested same-name tags.
func skipElement(lower, name string, from int) int {
	depth, i := 1, from
	open, closeT := "<"+name, "</"+name
	for i < len(lower) {
		o := strings.Index(lower[i:], open)
		c := strings.Index(lower[i:], closeT)
		if c < 0 {
			return len(lower)
		}
		if o >= 0 && o < c && isTagBoundary(lower, i+o+len(open)) {
			depth++
			i += o + len(open)
			continue
		}
		depth--
		i += c + len(closeT)
		if depth == 0 {
			if g := strings.IndexByte(lower[i:], '>'); g >= 0 {
				return i + g + 1
			}
			return len(lower)
		}
	}
	return len(lower)
}

func isTagBoundary(s string, i int) bool {
	return i >= len(s) || s[i] == '>' || s[i] == ' ' || s[i] == '/' || s[i] == '\n' || s[i] == '\t'
}

// htmlToText returns the page title and its main content as text.
func htmlToText(src string, base *url.URL) (title, text string) {
	lower := strings.ToLower(src)
	if i := strings.Index(lower, "<title"); i >= 0 {
		if g := strings.IndexByte(lower[i:], '>'); g >= 0 {
			if e := strings.Index(lower[i+g:], "</title"); e >= 0 {
				title = strings.Join(strings.Fields(html.UnescapeString(src[i+g+1:i+g+e])), " ")
			}
		}
	}
	// Prefer <main>, then <article>, when the page has one.
	for _, tag := range []string{"main", "article"} {
		if i := strings.Index(lower, "<"+tag); i >= 0 && isTagBoundary(lower, i+len(tag)+1) {
			end := skipElement(lower, tag, strings.IndexByte(lower[i:], '>')+i+1)
			if end-i > 200 {
				src, lower = src[i:end], lower[i:end]
				break
			}
		}
	}

	w := &htmlWriter{}
	i := 0
	for i < len(src) {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			w.text(html.UnescapeString(src[i:]))
			break
		}
		if lt > 0 {
			w.text(html.UnescapeString(src[i : i+lt]))
			i += lt
		}
		if strings.HasPrefix(src[i:], "<!--") {
			e := strings.Index(src[i+4:], "-->")
			if e < 0 {
				break
			}
			i += 4 + e + 3
			continue
		}
		gt := strings.IndexByte(src[i:], '>')
		if gt < 0 {
			break
		}
		inner := src[i+1 : i+gt]
		i += gt + 1
		if inner == "" || inner[0] == '!' || inner[0] == '?' {
			continue
		}
		closing := inner[0] == '/'
		inner = strings.TrimPrefix(inner, "/")
		name := inner
		if k := strings.IndexAny(inner, " \t\n/"); k >= 0 {
			name = inner[:k]
		}
		name = strings.ToLower(name)
		if name == "" || !(name[0] >= 'a' && name[0] <= 'z') {
			w.text("<")
			continue
		}
		attrs := map[string]string{}
		if !closing && len(inner) > len(name) {
			attrs = tagAttrs(inner[len(name):])
		}
		if !closing && htmlSkipTags[name] && !strings.HasSuffix(inner, "/") {
			i = skipElement(lower, name, i)
			continue
		}
		if closing {
			w.closeTag(name, base)
		} else {
			w.openTag(name, attrs)
		}
	}
	text = strings.TrimSpace(htmlBlankRe.ReplaceAllString(w.b.String(), "\n\n"))
	lines := strings.Split(text, "\n")
	for k, l := range lines {
		lines[k] = strings.TrimRight(l, " ")
	}
	text = htmlBlankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return title, text
}

func (w *htmlWriter) openTag(name string, attrs map[string]string) {
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.newline(2)
		w.b.WriteString(strings.Repeat("#", int(name[1]-'0')) + " ")
	case "br":
		w.newline(1)
	case "li":
		w.newline(1)
		w.b.WriteString(strings.Repeat("  ", max(len(w.listOL)-1, 0)))
		if n := len(w.listOL); n > 0 && w.listOL[n-1] > 0 {
			w.b.WriteString(strconv.Itoa(w.listOL[n-1]) + ". ")
			w.listOL[n-1]++
		} else {
			w.b.WriteString("- ")
		}
	case "ul":
		w.newline(1)
		w.listOL = append(w.listOL, 0)
	case "ol":
		w.newline(1)
		w.listOL = append(w.listOL, 1)
	case "pre":
		w.newline(2)
		w.b.WriteString("```\n")
		w.inPre = true
	case "code":
		if !w.inPre {
			w.b.WriteByte('`')
		}
	case "a":
		w.links = append(w.links, linkMark{start: w.b.Len(), href: attrs["href"]})
	case "img":
		if alt := strings.TrimSpace(attrs["alt"]); alt != "" {
			w.text("[image: " + alt + "]")
		}
	case "td", "th":
		w.text(" | ")
	default:
		if htmlBlockTags[name] {
			w.newline(2)
		}
	}
}

func (w *htmlWriter) closeTag(name string, base *url.URL) {
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.newline(2)
	case "ul", "ol":
		if n := len(w.listOL); n > 0 {
			w.listOL = w.listOL[:n-1]
		}
		w.newline(2)
	case "li":
		w.newline(1)
	case "pre":
		w.inPre = false
		w.newline(1)
		w.b.WriteString("```")
		w.newline(2)
	case "code":
		if !w.inPre {
			w.b.WriteByte('`')
		}
	case "a":
		n := len(w.links)
		if n == 0 {
			return
		}
		m := w.links[n-1]
		w.links = w.links[:n-1]
		s := w.b.String()
		label := strings.TrimSpace(s[m.start:])
		href := strings.TrimSpace(m.href)
		if label == "" || href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			return
		}
		if ref, err := url.Parse(href); err == nil && base != nil {
			href = base.ResolveReference(ref).String()
		}
		w.b.Reset()
		w.b.WriteString(s[:m.start] + "[" + label + "](" + href + ")")
	default:
		if htmlBlockTags[name] {
			w.newline(2)
		}
	}
}
