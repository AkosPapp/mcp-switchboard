package library

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// A tiny frontmatter codec: a --- block of `key: value` lines (values are
// compact JSON when they are not plain scalars, so YAML compatibility is
// preserved either way), then the Markdown body. Keys the library does not
// model (license, compatibility, hand-added metadata...) round-trip through
// Extras unchanged, so an external edit is never silently rewritten away.

type fmBlock struct {
	order  []string
	values map[string]string // raw value text per key
}

func parseFrontmatter(text string) (fm fmBlock, body string, ok bool) {
	fm.values = map[string]string{}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return fm, text, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		t := strings.TrimRight(lines[i], "\r")
		if t == "---" || t == "..." {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, text, false
	}
	for _, line := range lines[1:end] {
		t := strings.TrimRight(line, "\r")
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, " ") || strings.HasPrefix(t, "\t") || strings.HasPrefix(t, "-") {
			continue
		}
		i := strings.Index(t, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(t[:i])
		if key == "" {
			continue
		}
		if _, exists := fm.values[key]; !exists {
			fm.order = append(fm.order, key)
		}
		fm.values[key] = strings.TrimSpace(t[i+1:])
	}
	body = strings.Join(lines[end+1:], "\n")
	return fm, strings.TrimPrefix(body, "\n"), true
}

func (f fmBlock) str(key string) string {
	raw, ok := f.values[key]
	if !ok {
		return ""
	}
	if len(raw) >= 2 && (raw[0] == '"' || raw[0] == '\'') && raw[len(raw)-1] == raw[0] {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			return s
		}
		return strings.Trim(raw, "'")
	}
	return raw
}

func (f fmBlock) boolean(key string) bool {
	b, _ := strconv.ParseBool(f.values[key])
	return b
}

func (f fmBlock) json(key string) json.RawMessage {
	raw := f.values[key]
	if raw == "" || raw == "null" {
		return nil
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return nil
}

// serialize emits a frontmatter value: JSON-quoted strings, bare booleans, raw
// JSON for raw messages (nil drops the key).
func fmValue(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		if t == "" {
			return "", false
		}
		q, _ := json.Marshal(t)
		return string(q), true
	case bool:
		return strconv.FormatBool(t), true
	case json.RawMessage:
		if len(t) == 0 || string(t) == "null" {
			return "", false
		}
		return string(t), true
	}
	return "", false
}

type fmEntry struct {
	key   string
	value string
	emit  bool
}

func renderFrontmatter(entries []fmEntry, extras fmBlock, extraOrder []string, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	written := map[string]bool{}
	for _, e := range entries {
		if !e.emit || e.value == "" {
			continue
		}
		b.WriteString(e.key + ": " + e.value + "\n")
		written[e.key] = true
	}
	for _, k := range extraOrder {
		if written[k] {
			continue
		}
		b.WriteString(k + ": " + extras.values[k] + "\n")
	}
	b.WriteString("---\n")
	out := b.String() + body
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// fmStr quotes a plain string value for the frontmatter ("" drops the key).
func fmStr(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	q, _ := json.Marshal(s)
	return string(q), true
}

// ---------------------------------------------------------------- documents

func fileMTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime().UTC()
}

// readSkillDoc reads one SKILL.md; dirName is the fallback name (the
// directory), host marks scanned copies. A file without usable frontmatter is
// an error so callers can skip junk, but the name/body survive: only an empty
// body rejects.
func readSkillDoc(path, dirName, host string) (*SkillDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := skillDocFromText(string(data), dirName, host)
	if err != nil {
		return nil, err
	}
	doc.Path = path
	doc.Raw = string(data)
	doc.UpdatedAt = fileMTime(path)
	return doc, nil
}

func skillDocFromText(text, dirName, host string) (*SkillDoc, error) {
	fm, body, hasFM := parseFrontmatter(text)
	doc := &SkillDoc{}
	if hasFM {
		doc.ID = fm.str("id")
		doc.Name = fm.str("name")
		doc.Description = fm.str("description")
		doc.Auto = fm.boolean("auto")
		doc.License = fm.str("license")
		doc.Compatibility = fm.str("compatibility")
		doc.Source = fm.str("source")
		for _, k := range fm.order {
			switch k {
			case "id", "name", "description", "auto", "license", "compatibility", "source":
			default:
				if doc.Extras == nil {
					doc.Extras = map[string]string{}
				}
				doc.Extras[k] = fm.values[k]
			}
		}
	}
	if doc.Name == "" {
		doc.Name = dirName
	}
	doc.Body = strings.TrimRight(body, "\n")
	doc.Host = host
	if doc.Body == "" {
		return nil, ErrNotFound
	}
	return doc, nil
}

func renderSkillDoc(d *SkillDoc) string {
	entries := []fmEntry{{key: "name", value: d.Name, emit: true}}
	if v, ok := fmStr(d.Description); ok {
		entries = append(entries, fmEntry{key: "description", value: v, emit: true})
	}
	if d.Auto {
		entries = append(entries, fmEntry{key: "auto", value: "true", emit: true})
	}
	if d.ID != "" {
		v, _ := fmStr(d.ID)
		entries = append(entries, fmEntry{key: "id", value: v, emit: true})
	}
	if d.Host != "" && d.Source != "" {
		v, _ := fmStr(d.Source)
		entries = append(entries, fmEntry{key: "source", value: v, emit: true})
	}
	if d.License != "" {
		v, _ := fmStr(d.License)
		entries = append(entries, fmEntry{key: "license", value: v, emit: true})
	}
	if d.Compatibility != "" {
		v, _ := fmStr(d.Compatibility)
		entries = append(entries, fmEntry{key: "compatibility", value: v, emit: true})
	}
	var extras fmBlock
	extras.values = map[string]string{}
	for k, v := range d.Extras {
		extras.values[k] = v
	}
	return renderFrontmatter(entries, extras, sortedKeys(extras.values), d.Body)
}

func readPromptDoc(path string) (*PromptDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body, hasFM := parseFrontmatter(string(data))
	base := strings.TrimSuffix(strings.TrimSuffix(path, ".md"), string(os.PathSeparator)+"")
	name := base[strings.LastIndexAny(base, `/\`)+1:]
	doc := &PromptDoc{Slug: name, Body: strings.TrimRight(body, "\n")}
	if hasFM {
		doc.ID = fm.str("id")
		doc.Name = fm.str("name")
		doc.Description = fm.str("description")
		doc.Approval = fm.str("approval")
		doc.Model = fm.json("model")
		doc.Capabilities = fm.json("capabilities")
		doc.Budget = fm.json("budget")
		doc.IsDefault = fm.boolean("default")
		for _, k := range fm.order {
			switch k {
			case "id", "name", "description", "approval", "model", "capabilities", "budget", "default":
			default:
				if doc.Extras == nil {
					doc.Extras = map[string]string{}
				}
				doc.Extras[k] = fm.values[k]
			}
		}
	}
	if doc.Name == "" {
		doc.Name = doc.Slug
	}
	if strings.TrimSpace(doc.Body) == "" {
		return nil, ErrNotFound
	}
	doc.UpdatedAt = fileMTime(path)
	return doc, nil
}

func renderPromptDoc(d *PromptDoc) string {
	entries := []fmEntry{{key: "name", value: mustQuote(d.Name), emit: true}}
	if d.ID != "" {
		entries = append(entries, fmEntry{key: "id", value: mustQuote(d.ID), emit: true})
	}
	if v, ok := fmStr(d.Description); ok {
		entries = append(entries, fmEntry{key: "description", value: v, emit: true})
	}
	if d.Approval != "" {
		entries = append(entries, fmEntry{key: "approval", value: mustQuote(d.Approval), emit: true})
	}
	if v, ok := fmValue(d.Model); ok {
		entries = append(entries, fmEntry{key: "model", value: v, emit: true})
	}
	if v, ok := fmValue(d.Capabilities); ok {
		entries = append(entries, fmEntry{key: "capabilities", value: v, emit: true})
	}
	if v, ok := fmValue(d.Budget); ok {
		entries = append(entries, fmEntry{key: "budget", value: v, emit: true})
	}
	if d.IsDefault {
		entries = append(entries, fmEntry{key: "default", value: "true", emit: true})
	}
	var extras fmBlock
	extras.values = map[string]string{}
	for k, v := range d.Extras {
		extras.values[k] = v
	}
	return renderFrontmatter(entries, extras, sortedKeys(extras.values), d.Body)
}

func mustQuote(s string) string {
	v, _ := fmStr(s)
	if v == "" {
		return `""`
	}
	return v
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
