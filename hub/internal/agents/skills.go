package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Skills are reusable instructions with two doors in:
//
//   - the user types /name [arguments] as a message; the run loop swaps that
//     text for the skill's body when it builds the model's context (the stored
//     message stays exactly what was typed);
//   - for skills marked auto, the system prompt lists name and description and
//     the model may call switchboard.skill.load to read one in full. Only the
//     short listing costs tokens every turn; the body is loaded on demand.

const skillLoadName = "switchboard.skill.load"

// slashRE: "/name" alone, or followed by whitespace and arguments.
var slashRE = regexp.MustCompile(`^/([a-z0-9][a-z0-9_-]*)(?:\s+([\s\S]*))?$`)

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func checkSkillName(name string) error {
	if !skillNameRE.MatchString(name) {
		return fmt.Errorf("%w: skill name must be lowercase letters, digits, - or _ (at most 64), e.g. code-review", ErrInvalid)
	}
	return nil
}

func (m *Manager) ListSkills(ctx context.Context) ([]store.Skill, error) { return m.st.ListSkills(ctx) }

func (m *Manager) CreateSkill(ctx context.Context, in SkillInput) (*store.Skill, error) {
	name := strings.TrimSpace(in.Name)
	if err := checkSkillName(name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, fmt.Errorf("%w: body is required", ErrInvalid)
	}
	k, err := m.st.CreateSkill(ctx, store.Skill{
		Name: name, Description: strings.TrimSpace(in.Description), Body: in.Body, Auto: in.Auto,
	})
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeSkill})
	return &k, nil
}

func (m *Manager) UpdateSkill(ctx context.Context, id string, in SkillUpdate) (*store.Skill, error) {
	patch := store.SkillPatch{Description: in.Description, Body: in.Body, Auto: in.Auto}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := checkSkillName(n); err != nil {
			return nil, err
		}
		patch.Name = &n
	}
	if in.Body != nil && strings.TrimSpace(*in.Body) == "" {
		return nil, fmt.Errorf("%w: body must not be empty", ErrInvalid)
	}
	k, err := m.st.UpdateSkill(ctx, id, patch)
	if err != nil {
		return nil, err
	}
	m.publish(events.Event{Type: events.TypeSkill})
	return &k, nil
}

func (m *Manager) DeleteSkill(ctx context.Context, id string) error {
	if err := m.st.DeleteSkill(ctx, id); err != nil {
		return err
	}
	m.publish(events.Event{Type: events.TypeSkill})
	return nil
}

// autoSkills are the skills the model may load itself.
func (m *Manager) autoSkills(ctx context.Context) []store.Skill {
	all, err := m.st.ListSkills(ctx)
	if err != nil {
		m.log.Warn("could not list skills", "error", err)
		return nil
	}
	out := all[:0:0]
	for _, k := range all {
		if k.Auto {
			out = append(out, k)
		}
	}
	return out
}

// skillsPrompt is the listing appended to the system prompt; "" with no skills.
func skillsPrompt(skills []store.Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("You have skills: reusable instructions for particular kinds of task. When a task matches one, call ")
	b.WriteString(skillLoadName)
	b.WriteString(" with its name to read it, then follow it.\n\nAvailable skills:\n")
	for _, k := range skills {
		b.WriteString("- ")
		b.WriteString(k.Name)
		if k.Description != "" {
			b.WriteString(": ")
			b.WriteString(strings.Join(strings.Fields(k.Description), " "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// skillLoadTool is offered (see buildCatalog) only while some skill is auto.
// It is not in sbTools: it needs no capability and is not a hub-tools entry.
var skillLoadTool = &sbTool{
	name: skillLoadName,
	desc: "Load one of your skills by name and return its instructions. The available skills are listed in your system prompt.",
	schema: obj([]string{"name"}, map[string]any{
		"name": typ("string", "the skill's name, exactly as listed")}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: bp(false)},
	visible: func(store.Capabilities) bool { return true },
	run: func(m *Manager, ctx context.Context, _ *callCtx, args map[string]any) (any, error) {
		name := strings.TrimSpace(argStr(args, "name"))
		for _, k := range m.autoSkills(ctx) {
			if k.Name == name {
				return k.Body, nil
			}
		}
		return nil, fmt.Errorf("no skill named %q", name)
	},
}

// expandSlashSkills rewrites user messages that start with /name (a known
// skill) into the skill's instructions followed by whatever came after the
// name. Only what is sent to the model changes; the stored message does not.
func expandSlashSkills(msgs []llm.Message, skills []store.Skill) []llm.Message {
	if len(skills) == 0 {
		return msgs
	}
	byName := make(map[string]store.Skill, len(skills))
	for _, k := range skills {
		byName[k.Name] = k
	}
	for i, msg := range msgs {
		if msg.Role != llm.RoleUser || len(msg.Content) == 0 || msg.Content[0].Type != llm.BlockText {
			continue
		}
		m := slashRE.FindStringSubmatch(msg.Content[0].Text)
		if m == nil {
			continue
		}
		k, ok := byName[m[1]]
		if !ok {
			continue
		}
		rest := m[2]
		expanded := fmt.Sprintf("<skill name=%q>\n%s\n</skill>", k.Name, strings.TrimSpace(k.Body))
		if rest = strings.TrimSpace(rest); rest != "" {
			expanded += "\n\n" + rest
		}
		content := append([]llm.Block{}, msg.Content...)
		content[0].Text = expanded
		msgs[i].Content = content
	}
	return msgs
}
