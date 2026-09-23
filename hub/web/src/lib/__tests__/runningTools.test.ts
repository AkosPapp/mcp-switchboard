import { describe, expect, it } from "vitest";

import { runningLabel, runningToolNames } from "../runningTools";

describe("runningToolNames", () => {
  it("lists calls without a result, as-is, for parallel calls", () => {
    const names = runningToolNames(
      [
        { callId: "1", name: "run_command", done: false },
        { callId: "2", name: "fetch__fetch", done: false },
        { callId: "3", name: "filesystem__list_directory", done: true },
        { callId: "1", name: "run_command", done: false },
      ],
      new Set(),
      new Set(),
    );
    expect(names).toEqual(["run_command", "fetch__fetch"]);
    expect(runningLabel(names)).toBe("Running run_command, fetch__fetch…");
  });
  it("skips calls waiting for approval or already answered", () => {
    expect(
      runningToolNames([{ callId: "1", name: "a", done: false }, { callId: "2", name: "b", done: false }, { callId: "3", name: "c", done: false }], new Set(["1"]), new Set(["2"])),
    ).toEqual(["c"]);
  });
});
