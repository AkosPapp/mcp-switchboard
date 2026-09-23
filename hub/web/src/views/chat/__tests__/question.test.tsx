import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { AttentionTracker } from "../../../lib/attention";
import QuestionCard from "../QuestionCard";
import type { ApprovalItem, Chat } from "../types";

const soon = () => new Date(Date.now() + 60_000).toISOString();
const questions = [
  { question: "Which db?", header: "DB", options: [{ label: "sqlite" }, { label: "postgres", description: "bigger" }] },
  { question: "Which extras?", multiSelect: true, options: [{ label: "auth" }, { label: "cache" }] },
];

describe("QuestionCard", () => {
  it("sends one answer per question: options, then typed text", () => {
    const onAnswer = vi.fn();
    render(<QuestionCard questions={questions} expiresAt={soon()} onAnswer={onAnswer} onSkip={() => {}} />);
    const send = screen.getByRole("button", { name: "Send answers" }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);

    fireEvent.click(screen.getByLabelText(/postgres/));
    expect(send.disabled).toBe(true); // the second question is unanswered
    fireEvent.click(screen.getByLabelText("auth"));
    fireEvent.click(screen.getByLabelText("cache"));
    fireEvent.change(screen.getByLabelText("your own answer to: Which extras?"), { target: { value: "  logging " } });
    expect(send.disabled).toBe(false);
    fireEvent.click(send);
    expect(onAnswer).toHaveBeenCalledWith([["postgres"], ["auth", "cache", "logging"]]);
  });

  it("a typed answer replaces the chosen option of a single-choice question", () => {
    const onAnswer = vi.fn();
    render(<QuestionCard questions={[questions[0]]} expiresAt={soon()} onAnswer={onAnswer} onSkip={() => {}} />);
    fireEvent.click(screen.getByLabelText("sqlite"));
    fireEvent.change(screen.getByLabelText("your own answer to: Which db?"), { target: { value: "duckdb" } });
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(onAnswer).toHaveBeenCalledWith([["duckdb"]]);
  });

  it("Skip declines", () => {
    const onSkip = vi.fn();
    render(<QuestionCard questions={[questions[0]]} expiresAt={soon()} onAnswer={() => {}} onSkip={onSkip} />);
    fireEvent.click(screen.getByRole("button", { name: "Skip" }));
    expect(onSkip).toHaveBeenCalled();
  });
});

describe("attention", () => {
  const chat = { id: "c1", agentId: "a1", title: "T", updatedAt: "2026-01-01T00:00:00Z", archivedAt: null, kind: "human", peerAgentId: null } as unknown as Chat;
  const item = (over: Partial<ApprovalItem>): ApprovalItem => ({
    runId: "r", chatId: "c1", agentId: "a1", agentName: "a", chatTitle: "T", callId: "k", tool: "x", arguments: {}, expiresAt: "", ...over,
  });

  it("marks a chat with a question as such, with the question as the body, and notifies once", () => {
    const t = new AttentionTracker({});
    t.update({ approvals: [], chats: [chat], agents: [], openChatId: null, visible: true }); // baseline
    const out = t.update({
      approvals: [item({ tool: "switchboard.user.ask", questions: [{ question: "Which db?" }] })],
      chats: [chat], agents: [], openChatId: null, visible: true,
    });
    expect(out.marks.c1.reason).toBe("question");
    expect(out.marks.c1.body).toBe("Which db?");
    expect(out.notify).toHaveLength(1);
  });
});
