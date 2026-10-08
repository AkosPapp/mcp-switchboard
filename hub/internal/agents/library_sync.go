package agents

// The library (prompts/*.md, skills/<name>/SKILL.md under $DATA_DIR) is the
// record; the SQLite rows for skills and profiles are its index — they carry
// the stable ids the chat/agent rows reference. These helpers keep the two in
// step: reads reconcile disk-first, so an edit made with a text editor between
// turns is picked up and a file deleted on disk deletes its row; every Manager
// mutation writes the file back, so the two never drift while the hub runs.
// All of it is a no-op when Options.Library is nil, which is the old
// SQLite-only behaviour.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/library"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// reconcileLibrary syncs the skills and prompt files on disk into the index.
// Called at the start of planTurn and by every skills/profiles API entry.
func (m *Manager) reconcileLibrary(ctx context.Context) {
	if m.lib == nil {
		return
	}
	m.reconcileSkills(ctx)
	m.reconcilePrompts(ctx)
}

func (m *Manager) reconcileSkills(ctx context.Context) {
	docs, err := m.lib.ListSkills()
	if err != nil {
		m.log.Warn("library: listing skills failed", "error", err)
		return
	}
	rows, err := m.st.ListSkills(ctx)
	if err != nil {
		return
	}
	byID := make(map[string]store.Skill, len(rows))
	byName := make(map[string]store.Skill, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
		byName[r.Name] = r
	}
	seen := make(map[string]bool, len(docs))
	changed := false
	for _, d := range docs {
		row, ok := byID[d.ID]
		if !ok {
			row, ok = byName[d.Name]
		}
		if !ok {
			created, err := m.st.CreateSkill(ctx, store.Skill{
				Name: d.Name, Description: d.Description, Body: d.Body, Auto: d.Auto,
			})
			if err != nil {
				m.log.Warn("library: could not adopt a skill file", "file", d.Name, "error", err)
				continue
			}
			d.ID = created.ID // stamp the adopted id so the pair is stable
			if _, err := m.lib.WriteSkill(d, d.Name); err != nil {
				m.log.Warn("library: could not stamp a skill id", "file", d.Name, "error", err)
			}
			seen[created.ID] = true
			changed = true
			continue
		}
		seen[row.ID] = true
		if row.Name != d.Name || row.Description != d.Description || row.Body != d.Body || row.Auto != d.Auto {
			patch := store.SkillPatch{Description: &d.Description, Body: &d.Body, Auto: &d.Auto}
			if row.Name != d.Name {
				n := d.Name
				patch.Name = &n
			}
			if _, err := m.st.UpdateSkill(ctx, row.ID, patch); err != nil {
				m.log.Warn("library: could not sync a skill edit", "file", d.Name, "error", err)
				continue
			}
			changed = true
		}
		if d.ID != row.ID || d.Name != row.Name { // file stale (id or pre-rename path): rewrite it
			d.ID = row.ID
			if _, err := m.lib.WriteSkill(d, d.Name); err != nil {
				m.log.Warn("library: could not restamp a skill file", "file", d.Name, "error", err)
			}
		}
	}
	for _, r := range rows {
		if !seen[r.ID] {
			if err := m.st.DeleteSkill(ctx, r.ID); err == nil {
				changed = true
			}
		}
	}
	if changed {
		m.publish(events.Event{Type: events.TypeSkill})
	}
}

func (m *Manager) reconcilePrompts(ctx context.Context) {
	docs, err := m.lib.ListPrompts()
	if err != nil {
		m.log.Warn("library: listing prompts failed", "error", err)
		return
	}
	rows, err := m.st.ListProfiles(ctx)
	if err != nil {
		return
	}
	byID := make(map[string]store.Profile, len(rows))
	byName := make(map[string]store.Profile, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
		byName[r.Name] = r
	}
	seen := make(map[string]bool, len(docs))
	changed := false
	resync := map[string]store.Profile{}
	for _, d := range docs {
		row, ok := byID[d.ID]
		if !ok {
			row, ok = byName[d.Name]
		}
		if !ok {
			caps := decodeCaps(d.Capabilities)
			created, err := m.st.CreateProfile(ctx, store.Profile{
				Name: d.Name, Description: d.Description, SystemPrompt: d.Body, Model: d.Model,
				Capabilities: caps, Approval: approvalOrDefault(d.Approval),
				Budget: budgetOrDefault(d.Budget), IsDefault: d.IsDefault,
			})
			if err != nil {
				if !existsErr(err) {
					m.log.Warn("library: could not adopt a prompt file", "file", d.Slug, "error", err)
				}
				continue
			}
			d.ID = created.ID
			if _, err := m.lib.WritePrompt(d); err != nil {
				m.log.Warn("library: could not stamp a prompt id", "file", d.Slug, "error", err)
			}
			seen[created.ID] = true
			changed = true
			continue
		}
		seen[row.ID] = true
		caps := decodeCaps(d.Capabilities)
		approval := approvalOrDefault(d.Approval)
		budget := budgetOrDefault(d.Budget)
		if row.Name != d.Name || row.Description != d.Description || row.SystemPrompt != d.Body ||
			string(row.Model) != string(d.Model) || row.Approval != approval ||
			string(row.Budget) != string(budget) || row.Capabilities != caps {
			patch := store.ProfilePatch{
				Name: ptrIfDiff(row.Name, d.Name), Description: ptrIfDiff(row.Description, d.Description),
				SystemPrompt: ptrIfDiff(row.SystemPrompt, d.Body), Approval: ptrIfDiff(row.Approval, approval),
				Capabilities: capsPtrIfDiff(row.Capabilities, caps),
			}
			if string(row.Model) != string(d.Model) {
				if len(d.Model) == 0 {
					patch.ClearModel = true
				} else {
					patch.Model = d.Model
				}
			}
			if string(row.Budget) != string(budget) {
				patch.Budget = budget
			}
			updated, err := m.st.UpdateProfile(ctx, row.ID, patch)
			if err != nil {
				m.log.Warn("library: could not sync a prompt edit", "file", d.Slug, "error", err)
				continue
			}
			resync[row.ID] = updated
			changed = true
		}
		if d.ID != row.ID {
			d.ID = row.ID
			if _, err := m.lib.WritePrompt(d); err != nil {
				m.log.Warn("library: could not restamp a prompt id", "file", d.Slug, "error", err)
			}
		}
	}
	for _, r := range rows {
		if !seen[r.ID] {
			if err := m.st.DeleteProfile(ctx, r.ID); err == nil {
				changed = true
			}
		}
	}
	// exactly one default: the first file flagging default=true (ListPrompts
	// sorts defaults first) wins over the row index's notion.
	if want, ok := defaultFileID(docs); ok {
		if rows, err := m.st.ListProfiles(ctx); err == nil && !profileDefaultIs(rows, want) {
			if _, err := m.st.SetDefaultProfile(ctx, want); err == nil {
				changed = true
			} else {
				m.log.Warn("library: could not move the profile default", "id", want, "error", err)
			}
		}
	}
	for id, p := range resync {
		pp := p
		m.syncProfileAgents(ctx, &pp)
		if _, err := m.lib.GetPrompt(id); err == nil && !p.IsDefault {
			// the edit changed fields the file also carries: refresh the file
			m.mirrorPromptFile(p)
		}
	}
	if changed {
		m.publish(events.Event{Type: events.TypeProfile})
	}
}

func defaultFileID(docs []library.PromptDoc) (string, bool) {
	for _, d := range docs {
		if d.IsDefault {
			return d.ID, true
		}
	}
	return "", false
}

func profileDefaultIs(rows []store.Profile, id string) bool {
	for _, r := range rows {
		if r.IsDefault {
			return r.ID == id
		}
	}
	return id == ""
}

func existsErr(err error) bool { return isNameTaken(err) || errors.Is(err, library.ErrExists) }

func ptrIfDiff(old, neu string) *string {
	if old == neu {
		return nil
	}
	return &neu
}

func capsPtrIfDiff(old, neu store.Capabilities) *store.Capabilities {
	if old == neu {
		return nil
	}
	return &neu
}

func decodeCaps(raw json.RawMessage) store.Capabilities {
	var c store.Capabilities
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

func encodeCaps(c store.Capabilities) json.RawMessage {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	return raw
}

func approvalOrDefault(a string) string {
	if a == "" {
		return store.ApprovalDestructive
	}
	return a
}

func budgetOrDefault(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// ----------------------------------------------------------------- mirroring

// mirrorSkillFile rewrites a skill's SKILL.md from its row, preserving the
// frontmatter extras (license, compatibility, hand-additions) the row cannot
// model. oldName is the name before a rename (empty when nothing moved).
func (m *Manager) mirrorSkillFile(k store.Skill, oldName string) {
	if m.lib == nil {
		return
	}
	doc := library.SkillDoc{ID: k.ID, Name: k.Name, Description: k.Description, Auto: k.Auto, Body: k.Body}
	if cur, err := m.lib.GetSkill(oldName); err == nil && cur != nil {
		doc.License, doc.Compatibility, doc.Extras = cur.License, cur.Compatibility, cur.Extras
	} else if cur, err := m.lib.GetSkill(k.Name); err == nil && cur != nil {
		doc.License, doc.Compatibility, doc.Extras = cur.License, cur.Compatibility, cur.Extras
	}
	from := oldName
	if from == "" {
		from = k.Name // update-in-place: pass the current name as the old one
	}
	if _, err := m.lib.WriteSkill(doc, from); err != nil {
		m.log.Warn("library: could not mirror a skill", "name", k.Name, "error", err)
	}
}

func (m *Manager) removeSkillFile(name string) {
	if m.lib == nil {
		return
	}
	if err := m.lib.DeleteSkill(name); err != nil && !notFoundErr(err) {
		m.log.Warn("library: could not delete a mirrored skill", "name", name, "error", err)
	}
}

func (m *Manager) mirrorPromptFile(p store.Profile) {
	if m.lib == nil {
		return
	}
	doc, _ := m.lib.GetPrompt(p.ID) // keep slug + extras when the pair already exists
	if doc == nil {
		doc = &library.PromptDoc{ID: p.ID}
	}
	doc.Name, doc.Description, doc.Body = p.Name, p.Description, p.SystemPrompt
	doc.Model, doc.Capabilities, doc.Budget = p.Model, encodeCaps(p.Capabilities), p.Budget
	doc.Approval, doc.IsDefault = p.Approval, p.IsDefault
	if _, err := m.lib.WritePrompt(*doc); err != nil {
		m.log.Warn("library: could not mirror a prompt", "name", p.Name, "error", err)
	}
}

func (m *Manager) removePromptFile(id string) {
	if m.lib == nil {
		return
	}
	if err := m.lib.DeletePrompt(id); err != nil && !notFoundErr(err) {
		m.log.Warn("library: could not delete a mirrored prompt", "id", id, "error", err)
	}
}

func notFoundErr(err error) bool {
	return errors.Is(err, library.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// -------------------------------------------------------------------- persona

// PersonaPrompt returns the persona prompt (prompts/base.md), seeding the
// built-in default on first read.
func (m *Manager) PersonaPrompt() (string, error) {
	if m.lib == nil {
		return defaultPersonaFallback(), nil
	}
	return m.lib.BasePrompt()
}

func defaultPersonaFallback() string { return library.DefaultPersona }

// SetPersonaPrompt rewrites the persona prompt; every next turn leads with it.
func (m *Manager) SetPersonaPrompt(text string) error {
	if m.lib == nil {
		return fmt.Errorf("%w: the library is not enabled", ErrInvalid)
	}
	err := m.lib.SetBasePrompt(text)
	if err == nil {
		m.publish(events.Event{Type: events.TypeProfile})
	}
	return err
}

// ----------------------------------------------------------------- host skills

// HostSkillView is one scanned skill as the console shows it.
type HostSkillView struct {
	Host        string `json:"host"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	Shadowed    bool   `json:"shadowed"` // a managed skill of the same name wins
}

// ListHostSkills returns every skill scanned from client hosts and stored
// read-only under skills/hosts/.
func (m *Manager) ListHostSkills(ctx context.Context) ([]HostSkillView, error) {
	if m.lib == nil {
		return nil, nil
	}
	docs, err := m.lib.ListHostSkills()
	if err != nil {
		return nil, err
	}
	managed := map[string]bool{}
	if rows, err := m.st.ListSkills(ctx); err == nil {
		for _, r := range rows {
			managed[r.Name] = true
		}
	}
	out := make([]HostSkillView, 0, len(docs))
	for _, d := range docs {
		out = append(out, HostSkillView{
			Host: d.Host, Name: d.Name, Description: d.Description,
			Source: d.Source, Path: d.Path, Shadowed: managed[d.Name],
		})
	}
	return out, nil
}

// ImportHostSkill copies one scanned skill into the managed library; the hub
// copy then owns the name and later rescans stay read-only mirrors.
func (m *Manager) ImportHostSkill(ctx context.Context, host, name string) (store.Skill, error) {
	if m.lib == nil {
		return store.Skill{}, fmt.Errorf("%w: the library is not enabled", ErrInvalid)
	}
	d, err := m.lib.GetHostSkill(host, name)
	if err != nil {
		return store.Skill{}, fmt.Errorf("%w: no such host skill", ErrNotFound)
	}
	k, err := m.CreateSkill(ctx, SkillInput{Name: d.Name, Description: d.Description, Body: d.Body})
	if err != nil {
		return store.Skill{}, err
	}
	return *k, nil
}

// RawSkill returns the SKILL.md text of one managed skill.
func (m *Manager) RawSkill(ctx context.Context, id string) (string, error) {
	if m.lib == nil {
		return "", fmt.Errorf("%w: the library is not enabled", ErrInvalid)
	}
	k, err := m.st.GetSkill(ctx, id)
	if err != nil || k == nil {
		return "", ErrNotFound
	}
	d, err := m.lib.GetSkill(k.Name)
	if err != nil || d.Raw == "" {
		return "", ErrNotFound
	}
	return d.Raw, nil
}

// PutRawSkill applies a whole SKILL.md to an existing skill: frontmatter name
// renames it, unknown frontmatter keys are preserved as-is.
func (m *Manager) PutRawSkill(ctx context.Context, id, text string) (*store.Skill, error) {
	if m.lib == nil {
		return nil, fmt.Errorf("%w: the library is not enabled", ErrInvalid)
	}
	k, err := m.st.GetSkill(ctx, id)
	if err != nil || k == nil {
		return nil, ErrNotFound
	}
	doc, err := library.ParseSkillMarkdown(text, k.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: not a readable SKILL.md: %v", ErrInvalid, err)
	}
	if err := checkSkillName(doc.Name); err != nil {
		return nil, err
	}
	desc, body := doc.Description, doc.Body
	if desc == "" {
		desc = k.Description
	}
	up := SkillUpdate{Description: &desc, Body: &body}
	if doc.Name != k.Name {
		n := doc.Name
		up.Name = &n
	}
	if _, err := m.UpdateSkill(ctx, id, up); err != nil {
		return nil, err
	}
	// Rewrite the file so the raw edit's frontmatter (license, compatibility,
	// extras) is exactly what lands on disk, after the mirror write.
	doc.ID = id
	if _, err := m.lib.WriteSkill(*doc, k.Name); err != nil {
		m.log.Warn("library: raw skill write failed after sync", "id", id, "error", err)
	}
	fresh, err := m.st.GetSkill(ctx, id)
	if err != nil || fresh == nil {
		return nil, ErrNotFound
	}
	return fresh, nil
}
