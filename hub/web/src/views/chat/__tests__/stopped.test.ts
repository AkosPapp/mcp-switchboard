import { describe, expect, it } from "vitest";

import { runStoppedText, stoppedRunNotice } from "../Composer";

const budget = { max_tokens: 1_000_000, max_cost_micros: 5_000_000, max_turns: 50 };

describe("runStoppedText", () => {
  it("explains a token budget stop and how to go on", () => {
    const text = runStoppedText({ status: "done", finishReason: "budget", limit: "max_tokens" }, budget);
    expect(text).toMatch(/^stopped: token budget \(1\.0M\)/);
    expect(text).toContain("re-sent every turn");
    expect(text).toContain("continue");
  });

  it("names the other limits", () => {
    expect(runStoppedText({ status: "done", finishReason: "budget", limit: "max_cost_micros" }, budget)).toContain("cost budget ($5.00)");
    expect(runStoppedText({ status: "done", finishReason: "budget", limit: "max_turns" }, budget)).toContain("turn limit (50)");
    expect(runStoppedText({ status: "done", finishReason: "budget", limit: "max_lifetime_cost_micros" }, budget)).toContain("lifetime cost");
  });

  it("stays silent on normal stops and errors (the failed line owns errors)", () => {
    expect(runStoppedText({ status: "done", finishReason: "stop", limit: "" }, budget)).toBeNull();
    expect(runStoppedText({ status: "error", finishReason: "error", limit: "" }, budget)).toBeNull();
    expect(runStoppedText({ status: "done", finishReason: "", limit: "" }, budget)).toBeNull();
  });

  it("notes a cancel", () => {
    expect(runStoppedText({ status: "cancelled", finishReason: "cancelled", limit: "" }, budget)).toMatch(/cancelled/);
  });
});

describe("stoppedRunNotice", () => {
  it("prefers the live run_done frame", () => {
    expect(
      stoppedRunNotice(
        { runId: "r2", status: "done", finishReason: "budget", limit: "max_tokens" },
        { id: "r1", status: "done", finishReason: "stop", usage: {} },
      ),
    ).toEqual({ status: "done", finishReason: "budget", limit: "max_tokens" });
  });

  it("falls back to the fetched row and ignores active runs", () => {
    expect(stoppedRunNotice(null, { id: "r1", status: "running", finishReason: null, usage: {} })).toBeNull();
    expect(stoppedRunNotice(null, { id: "r1", status: "done", finishReason: "budget", usage: { limit: "max_turns" } })).toEqual({
      status: "done",
      finishReason: "budget",
      limit: "max_turns",
    });
  });
});
