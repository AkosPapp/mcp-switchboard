package agents

// The library tools: editing the hub's own prompts and skills from the model's
// side. They are first-class (offered to every run while the library is on),
// unlike the old optimize-only set they replace, but writes are annotated
// destructive so the approval gate still stands between a model and live,
// shared state. Everything lands on disk under $DATA_DIR as SKILL.md / *.md.

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

var libAnnRead = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)}
var libAnnWrite = &mcp.ToolAnnotations{DestructiveHint: bp(true), OpenWorldHint: bp(false)}
var libVisible = func(store.Capabilities) bool { return true }

func init() {
	for _, t := range []*sbTool{
		skillListTool, skillCreateTool, skillSetTool, skillDeleteTool, skillRawTool,
		promptListTool, promptRawTool, promptSetRawTool, personaGetTool, personaSetTool,
	} {
		registerExtraTool(t, nil)
	}
}

var skillListTool = &sbTool{
	name:   "switchboard.skill.list",
	desc:   "List the hub's managed skills with id, name, description and auto-load flags (the SKILL.md library under the hub's data dir). Read-only. Example: skill.list {}",
	schema: obj(nil, map[string]any{}),
	ann:    libAnnRead, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, _ map[string]any) (any, error) {
		skills, err := m.ListSkills(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(skills))
		for _, sk := range skills {
			out = append(out, map[string]any{
				"skill_id": sk.ID, "name": sk.Name, "description": sk.Description,
				"auto": sk.Auto, "body_chars": utf8.RuneCountInString(sk.Body),
			})
		}
		return map[string]any{"skills": out}, nil
	},
}

var skillCreateTool = &sbTool{
	name: "switchboard.skill.create",
	desc: "Create a managed skill (stored as SKILL.md in the hub library) from a name, description and the instruction body. Set auto=true to let models load it on their own. Names are lowercase slugs. Example: skill.create {name: \"release-notes\", description: \"Draft release notes\", body: \"...\", auto: false}",
	schema: obj([]string{"name", "body"}, map[string]any{
		"name":        typ("string", "slug: lowercase letters, digits and single hyphens"),
		"description": typ("string", "one line telling models when to load it"),
		"body":        typ("string", "the instructions (the SKILL.md body)"),
		"auto":        typ("boolean", "also offered to models for self-loading; default false"),
	}),
	ann: libAnnWrite, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		auto, _ := argBool(args, "auto")
		k, err := m.CreateSkill(ctx, SkillInput{
			Name: argStr(args, "name"), Description: argStr(args, "description"), Body: argStr(args, "body"), Auto: auto,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "skill_id": k.ID, "name": k.Name}, nil
	},
}

var skillSetTool = &sbTool{
	name: "switchboard.skill.set",
	desc: "Replace a managed skill's description and/or body (identified by skill_id or name). It takes effect for every chat on their next turn — agree the exact new text with the user first. Example: skill.set {skill_id: \"sk_1\", body: \"...\"}",
	schema: obj(nil, map[string]any{
		"skill_id":    typ("string", "from skill.list (or pass name instead)"),
		"name":        typ("string", "the skill's name, if skill_id is unknown"),
		"description": typ("string", "new one-line description; omitted leaves it"),
		"body":        typ("string", "complete new instruction body; omitted leaves it"),
		"auto":        typ("boolean", "new auto flag; omitted leaves it"),
	}),
	ann: libAnnWrite, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		id := strings.TrimSpace(argStr(args, "skill_id"))
		name := strings.TrimSpace(argStr(args, "name"))
		if id == "" && name != "" {
			skills, err := m.ListSkills(ctx)
			if err != nil {
				return nil, err
			}
			for _, sk := range skills {
				if sk.Name == name {
					id = sk.ID
					break
				}
			}
		}
		desc, hasDesc := args["description"].(string)
		body, hasBody := args["body"].(string)
		auto, hasAuto := args["auto"].(bool)
		if id == "" { // upsert: nothing matched, so create what was requested
			if name == "" || !hasBody {
				return nil, fmt.Errorf("%w: creating needs name and body; updating needs skill_id", ErrInvalid)
			}
			k, err := m.CreateSkill(ctx, SkillInput{Name: name, Description: desc, Body: body, Auto: auto})
			if err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "skill_id": k.ID, "name": k.Name, "created": true}, nil
		}
		up := SkillUpdate{}
		if hasDesc {
			up.Description = &desc
		}
		if hasBody {
			up.Body = &body
		}
		if hasAuto {
			up.Auto = &auto
		}
		sk, err := m.UpdateSkill(ctx, id, up)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "skill_id": sk.ID, "name": sk.Name}, nil
	},
}

var skillDeleteTool = &sbTool{
	name:   "switchboard.skill.delete",
	desc:   "Delete a managed skill by name. The user must have asked for that skill by name — this removes it from the hub library and every chat immediately. Example: skill.delete {name: \"release-notes\"}",
	schema: obj([]string{"name"}, map[string]any{"name": typ("string", "the skill to remove")}),
	ann:    libAnnWrite, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		name := strings.TrimSpace(argStr(args, "name"))
		skills, err := m.ListSkills(ctx)
		if err != nil {
			return nil, err
		}
		for _, sk := range skills {
			if sk.Name == name {
				if err := m.DeleteSkill(ctx, sk.ID); err != nil {
					return nil, err
				}
				return map[string]any{"ok": true, "deleted": name}, nil
			}
		}
		return nil, fmt.Errorf("%w: no skill named %q", ErrNotFound, name)
	},
}

var skillRawTool = &sbTool{
	name:   "switchboard.skill.raw",
	desc:   "Return a managed skill's full SKILL.md text (frontmatter + body), the exact file the hub stores. Read-only. Example: skill.raw {skill_id: \"sk_1\"}",
	schema: obj([]string{"skill_id"}, map[string]any{"skill_id": typ("string", "from skill.list")}),
	ann:    libAnnRead, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		raw, err := m.RawSkill(ctx, argStr(args, "skill_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"markdown": raw}, nil
	},
}

var promptListTool = &sbTool{
	name:   "switchboard.prompt.list",
	desc:   "List the hub's prompts (profiles) with ids — each is the live system prompt every chat following it receives. Read-only. Example: prompt.list {}",
	schema: obj(nil, map[string]any{}),
	ann:    libAnnRead, visible: libVisible,
	run: func(m *Manager, ctx context.Context, cc *callCtx, _ map[string]any) (any, error) {
		profiles, err := m.ListProfiles(ctx)
		if err != nil {
			return nil, err
		}
		var chatProfileID *string
		if cc != nil && cc.rs != nil && cc.rs.chatID != "" {
			if c, err := m.st.GetChat(ctx, cc.rs.chatID); err == nil && c != nil {
				chatProfileID = c.ProfileID
			}
		}
		out := make([]map[string]any, 0, len(profiles))
		for _, p := range profiles {
			out = append(out, map[string]any{
				"profile_id": p.ID, "name": p.Name, "description": p.Description, "is_default": p.IsDefault,
				"this_chat_follows": chatProfileID != nil && *chatProfileID == p.ID,
				"prompt_chars":      utf8.RuneCountInString(p.SystemPrompt),
			})
		}
		return map[string]any{"profiles": out}, nil
	},
}

var promptRawTool = &sbTool{
	name:   "switchboard.prompt.raw",
	desc:   "Return a prompt's full Markdown (frontmatter + the system prompt body). Read-only. Example: prompt.raw {profile_id: \"01a0…\"}",
	schema: obj([]string{"profile_id"}, map[string]any{"profile_id": typ("string", "from prompt.list")}),
	ann:    libAnnRead, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		p, err := m.GetProfile(ctx, argStr(args, "profile_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"markdown": p.SystemPrompt, "name": p.Name, "description": p.Description}, nil
	},
}

var promptSetRawTool = &sbTool{
	name: "switchboard.prompt.set",
	desc: "REPLACE a prompt's (profile's) system prompt. It takes effect in every chat following that prompt on their next turn — show the user the new text and get an explicit OK in this chat BEFORE calling this. Include the whole replacement, not a diff. Example: prompt.set {profile_id: \"01a0…\", system_prompt: \"You are…\"}",
	schema: obj([]string{"profile_id", "system_prompt"}, map[string]any{
		"profile_id":    typ("string", "from prompt.list"),
		"system_prompt": typ("string", "the complete new system prompt"),
	}),
	ann: libAnnWrite, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		sp := argStr(args, "system_prompt")
		if strings.TrimSpace(sp) == "" {
			return nil, fmt.Errorf("%w: system_prompt must not be empty (an empty prompt means this tool is the wrong one)", ErrInvalid)
		}
		p, err := m.UpdateProfile(ctx, argStr(args, "profile_id"), ProfileUpdate{SystemPrompt: &sp})
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "profile_id": p.ID, "name": p.Name, "prompt_chars": utf8.RuneCountInString(p.SystemPrompt)}, nil
	},
}

var personaGetTool = &sbTool{
	name:   "switchboard.persona.get",
	desc:   "Return the hub's persona prompt — the text that leads EVERY chat's system prompt (prompts/base.md). Read-only. Example: persona.get {}",
	schema: obj(nil, map[string]any{}),
	ann:    libAnnRead, visible: libVisible,
	run: func(m *Manager, _ context.Context, _ *callCtx, _ map[string]any) (any, error) {
		text, err := m.PersonaPrompt()
		if err != nil {
			return nil, err
		}
		return map[string]any{"persona": text}, nil
	},
}

var personaSetTool = &sbTool{
	name: "switchboard.persona.set",
	desc: "REPLACE the hub's persona prompt — this changes the base identity of every chat on their next turn, model-wide. Only call it after the user has explicitly OKed the exact new text in this chat. Example: persona.set {persona: \"You are…\"}",
	schema: obj([]string{"persona"}, map[string]any{
		"persona": typ("string", "the complete new persona prompt"),
	}),
	ann: libAnnWrite, visible: libVisible,
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		if err := m.SetPersonaPrompt(argStr(args, "persona")); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	},
}
