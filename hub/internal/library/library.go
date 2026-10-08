// Package library is the hub's file store for the things an agent is made of:
// skills (SKILL.md, the opencode/Claude format) and system prompts (Markdown
// with a frontmatter index). It lives under $DATA_DIR so a person can inspect
// and edit it with any editor:
//
//	$DATA_DIR/skills/<name>/SKILL.md         managed skills (the hub's library)
//	$DATA_DIR/skills/hosts/<label>/<name>/…  skills scanned on a client host
//	$DATA_DIR/prompts/<slug>.md              profiles (reusable prompts)
//	$DATA_DIR/prompts/base.md                the persona prompt every turn leads with
//
// Files are the record. The SQLite rows that existed before this package
// remain as an index the orchestrator keeps in step (stable ids, chat/agent
// references); the agents package reconciles the two directions on every read
// and never lets the databases' copy answer a question the file already did.
//
// SKILL.md frontmatter uses the opencode grammar (name, description, license,
// compatibility, metadata) plus the hub's own keys (id, auto). Unknown keys
// survive rewrites, so a hand-added field is not quietly deleted.
package library

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned for skills and prompts that do not exist.
var ErrNotFound = errors.New("library: not found")

// ErrExists is returned when a write would clobber a different entry.
var ErrExists = errors.New("library: already exists")

var slugRX = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName is the opencode skill-name grammar (also used for prompt slugs).
func ValidName(name string) bool {
	return len(name) <= 64 && slugRX.MatchString(name)
}

// SkillDoc is one SKILL.md. Body is the Markdown below the frontmatter.
type SkillDoc struct {
	ID            string
	Name          string
	Description   string
	Auto          bool
	License       string
	Compatibility string
	Body          string
	// Host is set ("legion5") for scanned copies under skills/hosts/.
	Host string
	// Source is the client-reported discovery location for scanned copies.
	Source    string
	Path      string
	Raw       string // exact file contents, when read from disk
	UpdatedAt time.Time
	// Extras are frontmatter keys the library does not model (metadata and
	// anything hand-added); they survive rewrites.
	Extras map[string]string
}

// PromptDoc is one prompts/*.md file; Body is the system prompt itself.
type PromptDoc struct {
	ID           string
	Name         string
	Description  string
	Model        json.RawMessage
	Capabilities json.RawMessage
	Approval     string
	Budget       json.RawMessage
	IsDefault    bool
	Body         string
	UpdatedAt    time.Time

	Slug   string // file stem; set by read/write paths
	Extras map[string]string
}

// Library is bound to one data directory.
type Library struct {
	dir string
}

// New prepares <dir>/skills and <dir>/prompts below dir.
func New(dir string) (*Library, error) {
	for _, d := range []string{
		filepath.Join(dir, "skills"),
		filepath.Join(dir, "skills", "hosts"),
		filepath.Join(dir, "prompts"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("library: %w", err)
		}
	}
	return &Library{dir: dir}, nil
}

// Dir is the data directory the library was opened with.
func (l *Library) Dir() string { return l.dir }

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "lib_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------- SKILL.md

func (l *Library) skillPath(name string) string {
	return filepath.Join(l.dir, "skills", name, "SKILL.md")
}

func (l *Library) hostSkillPath(host, name string) string {
	return filepath.Join(l.dir, "skills", "hosts", host, name, "SKILL.md")
}

// ListSkills returns the managed skills sorted by name.
func (l *Library) ListSkills() ([]SkillDoc, error) {
	root := filepath.Join(l.dir, "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []SkillDoc
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "hosts" {
			continue
		}
		d, err := readSkillDoc(l.skillPath(e.Name()), e.Name(), "")
		if err != nil {
			continue // junk on disk must not take the whole list down
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListHostSkills returns the skills scanned from client hosts, grouped order:
// host, then name.
func (l *Library) ListHostSkills() ([]SkillDoc, error) {
	root := filepath.Join(l.dir, "skills", "hosts")
	hosts, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SkillDoc
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(root, h.Name()))
		if err != nil {
			continue
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			d, err := readSkillDoc(l.hostSkillPath(h.Name(), n.Name()), n.Name(), h.Name())
			if err != nil {
				continue
			}
			out = append(out, *d)
		}
	}
	return out, nil
}

// GetSkill reads one managed skill.
func (l *Library) GetSkill(name string) (*SkillDoc, error) {
	return readSkillDoc(l.skillPath(name), name, "")
}

// GetHostSkill reads one scanned skill.
func (l *Library) GetHostSkill(host, name string) (*SkillDoc, error) {
	if !ValidName(host) || !ValidName(name) {
		return nil, ErrNotFound
	}
	return readSkillDoc(l.hostSkillPath(host, name), name, host)
}

// WriteSkill creates or replaces a managed skill; the doc's Name decides the
// directory. ID is assigned when empty. Renaming (doc.Name differs from the
// optional oldName) moves the directory.
func (l *Library) WriteSkill(doc SkillDoc, oldName string) (*SkillDoc, error) {
	if !ValidName(doc.Name) {
		return nil, fmt.Errorf("library: invalid skill name %q", doc.Name)
	}
	if strings.TrimSpace(doc.Body) == "" {
		return nil, fmt.Errorf("library: skill body is required")
	}
	if doc.ID == "" {
		doc.ID = newID()
	}
	if oldName != "" && !ValidName(oldName) {
		return nil, fmt.Errorf("library: invalid old skill name %q", oldName)
	}
	if oldName == "" { // create: the target must not exist yet
		if _, err := os.Stat(l.skillPath(doc.Name)); err == nil {
			return nil, fmt.Errorf("%w: skill %q", ErrExists, doc.Name)
		}
	}
	dir := filepath.Dir(l.skillPath(doc.Name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	text := renderSkillDoc(&doc)
	if err := writeFileAtomic(l.skillPath(doc.Name), []byte(text)); err != nil {
		return nil, err
	}
	if oldName != "" && oldName != doc.Name {
		_ = os.RemoveAll(filepath.Dir(l.skillPath(oldName)))
	}
	doc.UpdatedAt = time.Now().UTC()
	return &doc, nil
}

// DeleteSkill removes a managed skill's directory.
func (l *Library) DeleteSkill(name string) error {
	if !ValidName(name) {
		return ErrNotFound
	}
	if _, err := os.Stat(l.skillPath(name)); err != nil {
		return ErrNotFound
	}
	return os.RemoveAll(filepath.Dir(l.skillPath(name)))
}

// PutHostSkills replaces one host's scanned set wholesale (the skills_update
// contract). Junk names are dropped, not refused.
func (l *Library) PutHostSkills(host string, items []RawSkill) error {
	if !ValidName(host) {
		return fmt.Errorf("library: invalid host label %q", host)
	}
	dir := filepath.Join(l.dir, "skills", "hosts", host)
	tmp := dir + ".new"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	for _, it := range items {
		if !ValidName(it.Name) || strings.TrimSpace(it.Content) == "" {
			continue
		}
		sd := filepath.Join(tmp, it.Name)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			return err
		}
		// The client ships whole SKILL.md files; parse, then canonicalise (the
		// directory name rules, the frame's source wins, frontmatter extras
		// like license survive through the doc round-trip).
		doc, err := skillDocFromText(it.Content, it.Name, host)
		if err != nil {
			doc = &SkillDoc{Body: it.Content}
		}
		doc.Name = it.Name
		doc.Host = host
		doc.Source = it.Source
		if doc.Description == "" {
			doc.Description = it.Description
		}
		if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(renderSkillDoc(doc)), 0o644); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// RawSkill is one client-scanned skill handed to PutHostSkills.
type RawSkill struct {
	Name        string
	Description string
	Source      string
	Content     string
}

// ---------------------------------------------------------------- prompts

// ListPrompts returns every prompt file except base.md, default first, then
// by name. base-only directories yield empty lists, not errors.
func (l *Library) ListPrompts() ([]PromptDoc, error) {
	root := filepath.Join(l.dir, "prompts")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []PromptDoc
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "base.md" {
			continue
		}
		d, err := readPromptDoc(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (l *Library) promptPathBySlug(slug string) string {
	return filepath.Join(l.dir, "prompts", slug+".md")
}

// GetPrompt reads a prompt by id (frontmatter key).
func (l *Library) GetPrompt(id string) (*PromptDoc, error) {
	all, err := l.ListPrompts()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, ErrNotFound
}

// WritePrompt creates or replaces a prompt (matched by ID when set, otherwise
// created). The slug derives from the name; a rename moves the file.
func (l *Library) WritePrompt(doc PromptDoc) (*PromptDoc, error) {
	slug := slugify(doc.Name)
	if slug == "" || slug == "base" {
		return nil, fmt.Errorf("library: invalid prompt name %q", doc.Name)
	}
	if doc.ID == "" {
		doc.ID = newID()
	}
	existing, err := l.GetPrompt(doc.ID)
	if err == nil && existing != nil {
		if err := os.Remove(l.promptPathBySlug(existing.Slug)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if existing == nil {
		// new prompt: the slug must be free
		if _, err := os.Stat(l.promptPathBySlug(slug)); err == nil {
			n, _ := readPromptDoc(l.promptPathBySlug(slug))
			if n != nil && n.ID != doc.ID {
				return nil, fmt.Errorf("%w: prompt %q", ErrExists, doc.Name)
			}
		}
	}
	doc.Slug = slug
	text := renderPromptDoc(&doc)
	if err := writeFileAtomic(l.promptPathBySlug(slug), []byte(text)); err != nil {
		return nil, err
	}
	doc.UpdatedAt = time.Now().UTC()
	return &doc, nil
}

// DeletePrompt removes a prompt by id.
func (l *Library) DeletePrompt(id string) error {
	d, err := l.GetPrompt(id)
	if err != nil {
		return err
	}
	return os.Remove(l.promptPathBySlug(d.Slug))
}

// EnsureDefaultPrompt makes id the only default (or clears all when empty).
func (l *Library) EnsureDefaultPrompt(id string) error {
	all, err := l.ListPrompts()
	if err != nil {
		return err
	}
	for i := range all {
		want := all[i].ID == id
		if all[i].IsDefault != want {
			all[i].IsDefault = want
			p := l.promptPathBySlug(all[i].Slug)
			if err := writeFileAtomic(p, []byte(renderPromptDoc(&all[i]))); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- persona

// DefaultPersona is written to prompts/base.md the first time a hub starts
// with this library: an opencode-style system prompt. The persona leads every
// turn's system prompt; profile text follows it.
const DefaultPersona = `You are opencode, a coding agent running inside mcp-switchboard. You work on a remote machine through a harness that carries the tool suite you already know (bash, read, write, edit, glob, grep); the orchestrator in front of you adds a few switchboard tools for planning, delegation and human questions.

# Tone and style
Be concise, direct, and to the point. Your text is shown on a command line: address the user, not your tools; no preamble, no postamble, fewer than four lines unless they ask for detail. Never commit unless explicitly asked.

# Doing tasks
Use the search tools before concluding, read what you will change before changing it, and mimic the surrounding code's conventions. Verify with tests or the project's own checks when they exist. Where a tool offers machine-readable output, take it.

# Environment
Your tools run on a client host, not on yourself: file paths are relative to that host's harness root, and the environment brief in this prompt describes the machine (user, OS, git state, writable set). Treat that brief as ground truth for the host and the repository's AGENTS.md-style instruction files as project law.

# Tools
Prefer dedicated tools over shelling out (read over cat, grep over shell grep); batch independent calls. bash waits up to a default timeout — long builds belong to the background-process tools. When you finish, stop: do not re-verify what you already saw.`

// BasePrompt reads prompts/base.md, seeding it with DefaultPersona when absent.
func (l *Library) BasePrompt() (string, error) {
	path := filepath.Join(l.dir, "prompts", "base.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeFileAtomic(path, []byte(DefaultPersona+"\n")); err != nil {
			return "", err
		}
		return DefaultPersona, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// SetBasePrompt overwrites prompts/base.md.
func (l *Library) SetBasePrompt(text string) error {
	path := filepath.Join(l.dir, "prompts", "base.md")
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("library: persona prompt must not be empty")
	}
	return writeFileAtomic(path, []byte(strings.TrimRight(text, "\n")+"\n"))
}

// ---------------------------------------------------------------- plumbing

func slugify(name string) string {
	var b []rune
	prevDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
			prevDash = false
		default:
			if !prevDash && len(b) > 0 {
				b = append(b, '-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(string(b), "-")
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "-")
	}
	return s
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".lib-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ParseSkillMarkdown parses SKILL.md text the way readSkillDoc does, without
// a file on disk (for raw-edit round trips).
func ParseSkillMarkdown(text, fallbackName string) (*SkillDoc, error) {
	return skillDocFromText(text, fallbackName, "")
}

// MarshalSkillDoc renders a skill back to SKILL.md text.
func MarshalSkillDoc(d *SkillDoc) string { return renderSkillDoc(d) }
