package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

// P1-A: instruction files carried by the client must reach the chat's system
// prompt — on separate lines (the regression this whole work package stems
// from), labelled with their paths, with the nested-precedence rule stated,
// and with no tool call involved.
func TestInstructionFilesReachTheSystemPrompt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "file_read"})
	conn := e.reg.Get("conn-box")
	if conn == nil {
		t.Fatal("no connection")
	}
	conn.SetInstructions(protocol.SanitizeInstructions([]protocol.InstructionFile{
		{Path: "AGENTS.md", Content: "# Isaac-SDG\n\nThe test command:\n\n    sbatch batch-isaac-apptainer.sh src/main.py\n"},
		{Path: "src/AGENTS.md", Content: "SENTINEL-DEEP-RULE\n"},
	}))
	a := e.agent("coder", func(in *CreateAgentInput) { in.SystemPrompt = "BASE-PROMPT" })
	chat := e.newChat(a.ID)

	v, err := e.m.ChatSystemPrompt(ctx, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	prompt := v.SystemPrompt
	for _, want := range []string{"BASE-PROMPT", "SENTINEL-DEEP-RULE", "sbatch batch-isaac-apptainer.sh", "box:AGENTS.md", "box:src/AGENTS.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt does not contain %q\n---\n%s", want, prompt)
		}
	}
	if strings.Index(prompt, "SENTINEL-DEEP-RULE") == -1 || !strings.Contains(prompt, "refines") {
		t.Error("precedence rule not stated")
	}
	// Line fidelity: the sbatch command must sit on its own line, and the two
	// instruction files must be separated (headings glued together was the bug).
	lines := strings.Split(prompt, "\n")
	var sbatchLines, headingLines int
	for _, ln := range lines {
		if strings.Contains(ln, "sbatch batch-isaac-apptainer.sh") {
			sbatchLines++
		}
		if strings.TrimSpace(ln) == "### box:AGENTS.md" || strings.TrimSpace(ln) == "### box:src/AGENTS.md" {
			headingLines++
		}
	}
	if sbatchLines != 1 {
		t.Errorf("sbatch command not on its own line (saw it on %d lines)", sbatchLines)
	}
	if headingLines != 2 {
		t.Errorf("instruction headings not on separate lines (saw %d)", headingLines)
	}
}

// A client with no instruction files changes nothing in the prompt.
func TestNoInstructionFilesMeansNoInjection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addServer("box", "", "harness", registry.ToolInfo{Name: "file_read"})
	a := e.agent("quiet", func(in *CreateAgentInput) { in.SystemPrompt = "BASE" })
	v, err := e.m.ChatSystemPrompt(ctx, e.newChat(a.ID).ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v.SystemPrompt, "Instruction files") {
		t.Errorf("injected despite no files: %q", v.SystemPrompt)
	}
}
