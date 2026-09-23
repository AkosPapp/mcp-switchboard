import { describe, expect, it } from "vitest";

import { fuzzyFilter, fuzzyMatch } from "../fuzzy";

describe("fuzzyMatch", () => {
  it("needs the characters in order, ignoring case and spaces", () => {
    expect(fuzzyMatch("gptoss", "gpt-oss:20b")).not.toBeNull();
    expect(fuzzyMatch("GPT OSS", "gpt-oss:20b")).not.toBeNull();
    expect(fuzzyMatch("ossgpt", "gpt-oss:20b")).toBeNull();
    expect(fuzzyMatch("xyz", "gpt-oss")).toBeNull();
    expect(fuzzyMatch("", "anything")).toEqual({ score: 0, positions: [] });
  });
  it("reports where it matched", () => {
    expect(fuzzyMatch("gs", "gpt-oss")!.positions).toEqual([0, 5]);
  });
  it("prefers consecutive runs and word starts", () => {
    const run = fuzzyMatch("qwen", "qwen3.5:latest")!;
    const scattered = fuzzyMatch("qwen", "quick-brown-wolf-eats-nuts")!;
    expect(run.score).toBeGreaterThan(scattered.score);
    const boundary = fuzzyMatch("os", "gpt-oss")!; // "o" starts a word
    const inside = fuzzyMatch("ps", "gpt-oss")!;
    expect(boundary.score).toBeGreaterThan(inside.score);
  });
});

describe("fuzzyFilter", () => {
  const models = ["llama3:8b", "gpt-oss:20b", "qwen3.5:latest", "gpt-4o"];
  it("keeps the order for an empty query", () => {
    expect(fuzzyFilter(models, " ", (m) => m)).toEqual(models);
  });
  it("drops non-matches and ranks the best first", () => {
    expect(fuzzyFilter(models, "gpt", (m) => m)).toEqual(["gpt-oss:20b", "gpt-4o"]);
    expect(fuzzyFilter(models, "q35", (m) => m)[0]).toBe("qwen3.5:latest");
    expect(fuzzyFilter(models, "zzz", (m) => m)).toEqual([]);
  });
});
