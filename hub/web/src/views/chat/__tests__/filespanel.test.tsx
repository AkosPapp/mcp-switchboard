import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderView } from "../../__tests__/helpers";
import FilesPanel, { touchedFiles } from "../FilesPanel";
import type { Message } from "../types";

const msg = (toolCalls: { name: string; arguments: unknown }[]): Message =>
  ({ role: "assistant", toolCalls } as unknown as Message);

describe("touchedFiles", () => {
  it("tracks new, edited, moved and deleted paths in order", () => {
    const files = touchedFiles([
      msg([{ name: "local__fs__file_write", arguments: { path: "a.go", content: "x" } }]),
      msg([{ name: "local__fs__file_write", arguments: { path: "a.go", content: "y" } }]),
      msg([{ name: "local__fs__file_write", arguments: { path: "b.go", content: "z" } }]),
      msg([{ name: "file_move", arguments: { src: "b.go", dst: "c/d.go" } }]),
      msg([{ name: "dir_remove", arguments: { path: "a.go" } }]),
    ]);
    expect(files).toEqual([
      { path: "a.go", op: "delete" },
      { path: "c/d.go", op: "move" },
    ]);
  });

  it("skips calls whose arguments do not name a path", () => {
    expect(touchedFiles([msg([{ name: "run_command", arguments: { cmd: "ls" } }])])).toEqual([]);
  });
});

describe("FilesPanel", () => {
  it("is hidden when nothing was touched", () => {
    renderView(<FilesPanel messages={[]} />);
    expect(screen.queryByTestId("files-panel")).toBeNull();
  });

  it("lists the changed files with an op each", () => {
    const one = [msg([{ name: "fs__tools__file_write", arguments: { path: "pkg/x.go" } }])];
    renderView(<FilesPanel messages={one} />);
    expect(screen.getByTestId("files-panel")).toBeTruthy();
    expect(screen.getByText("pkg/x.go")).toBeTruthy();
    expect(screen.getByText("new")).toBeTruthy();
  });
});
