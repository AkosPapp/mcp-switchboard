import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import MessageView, { type MessageActions } from "../MessageView";
import type { Message } from "../types";

const message = {
  id: "a1", chatId: "c", parentId: "u1", role: "assistant", content: [{ type: "text", text: "hi" }],
  toolCalls: null, toolResults: null, tokenInput: 10, tokenOutput: 20, costMicros: 0, latencyMs: 0,
  model: { provider: "ollama", model: "qwen" }, finishReason: "stop", runId: null, lastActiveChildId: null, createdAt: "",
} as Message;

describe("model picker", () => {
  it("regenerates the reply with the chosen model", () => {
    const regenerateWith = vi.fn();
    const actions = {
      busy: false, select: vi.fn(), regenerate: vi.fn(), edit: vi.fn(), branchHere: vi.fn(), regenerateWith,
      models: [
        { provider: "ollama", model: "qwen" },
        { provider: "ollama", model: "llama" },
      ],
    } as unknown as MessageActions;
    render(
      <MemoryRouter>
        <MessageView message={message} tools={[]} actions={actions} />
      </MemoryRouter>,
    );
    const trigger = screen.getByLabelText("regenerate with another model");
    expect(trigger.textContent).toContain("qwen");
    fireEvent.click(trigger);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "lla" } });
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    expect(regenerateWith).toHaveBeenCalledWith(message, { provider: "ollama", model: "llama" });
  });
});
