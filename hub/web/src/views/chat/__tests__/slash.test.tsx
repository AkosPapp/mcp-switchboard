import { fireEvent, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderView } from "../../__tests__/helpers";
import type { Skill } from "../../prompts/skills";
import Composer from "../Composer";
import { matchSkills } from "../SlashMenu";

const skill = (name: string, description = "", auto = true): Skill => ({
  id: name, name, description, body: "b", auto, createdAt: "", updatedAt: "",
});
const skills = [skill("code-review", "Review a change"), skill("summarize"), skill("review-notes")];

describe("matchSkills", () => {
  it("matches only a bare /prefix at the start", () => {
    expect(matchSkills(skills, "hello /sum")).toEqual([]);
    expect(matchSkills(skills, "/sum arg")).toEqual([]);
    expect(matchSkills(skills, "/").map((k) => k.name)).toEqual(["code-review", "summarize", "review-notes"]);
  });
  it("puts prefix matches first, then name or description matches", () => {
    expect(matchSkills(skills, "/review").map((k) => k.name)).toEqual(["review-notes", "code-review"]);
    expect(matchSkills(skills, "/zzz")).toEqual([]);
  });
});

describe("Composer slash menu", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input).replace(/^api\//, "").split("?")[0];
      const body = path === "skills" ? { skills } : path.startsWith("chats/") ? { draft: "" } : undefined;
      if (body === undefined) return { ok: false, status: 404, statusText: "nf", json: async () => ({}) } as Response;
      return { ok: true, status: 200, statusText: "OK", json: async () => body } as Response;
    }));
  });
  afterEach(() => vi.unstubAllGlobals());

  const setup = () =>
    renderView(<Composer chatId="c1" models={[]} running={false} onSend={async () => {}} onStop={() => {}} />);

  it("completes the command on click and on Tab, and Escape closes it", async () => {
    setup();
    const box = screen.getByLabelText("message") as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "/sum" } });
    const menu = await screen.findByTestId("slash-menu");
    expect(menu.textContent).toContain("/summarize");
    fireEvent.mouseDown(screen.getByText("/summarize"));
    expect(box.value).toBe("/summarize ");
    expect(screen.queryByTestId("slash-menu")).toBeNull();

    fireEvent.change(box, { target: { value: "/co" } });
    await screen.findByTestId("slash-menu");
    fireEvent.keyDown(box, { key: "Tab" });
    expect(box.value).toBe("/code-review ");

    fireEvent.change(box, { target: { value: "/r" } });
    await screen.findByTestId("slash-menu");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByTestId("slash-menu")).toBeNull();
  });
});
