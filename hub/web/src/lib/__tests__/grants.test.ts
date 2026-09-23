import { describe, expect, it } from "vitest";

import { grantFixture, serverFixture } from "../../views/graph/fixtures";
import {
  defaultChildGrants,
  effective,
  matches,
  permissionRows,
  refusedGrants,
  wouldWiden,
} from "../grants";

const ref = { label: "laptop", project: "", server: "files" };

describe("matches / effective", () => {
  it("treats * as any value in each field (A11)", () => {
    expect(matches({ label: "*", project: "*", server: "*" }, ref)).toBe(true);
    expect(matches({ label: "laptop", project: "", server: "*" }, ref)).toBe(true);
    expect(matches({ label: "other", project: "", server: "files" }, ref)).toBe(false);
  });

  it("lets a specific deny carve an exception out of a wildcard allow", () => {
    const grants = [
      grantFixture({ label: "*", project: "*", server: "*", allowed: true }),
      grantFixture({ server: "files", allowed: false }),
    ];
    expect(effective(grants, ref).allowed).toBe(false);
    expect(effective(grants, { ...ref, server: "git" }).allowed).toBe(true);
  });

  it("lets a deny win a tie and denies by default", () => {
    const grants = [
      grantFixture({ label: "*", allowed: true }),
      grantFixture({ project: "*", allowed: false }),
    ];
    expect(effective(grants, ref).allowed).toBe(false);
    expect(effective([], ref).allowed).toBe(false);
  });
});

describe("wouldWiden (A12/A13)", () => {
  it("never widens a root", () => {
    expect(wouldWiden(null, ref)).toBe(false);
  });
  it("widens when the parent lacks the ref", () => {
    expect(wouldWiden([], ref)).toBe(true);
    expect(wouldWiden([grantFixture({ allowed: false })], ref)).toBe(true);
    expect(wouldWiden([grantFixture()], ref)).toBe(false);
    expect(wouldWiden([grantFixture({ label: "*", project: "*", server: "*" })], ref)).toBe(false);
  });
});

describe("permissionRows", () => {
  it("joins grants with live servers and keeps offline grants (I3/A15)", () => {
    const rows = permissionRows(
      [
        grantFixture({ server: "files", source: "human" }),
        grantFixture({ server: "old", connected: false, source: "inherited", orphaned: true }),
      ],
      [serverFixture({ server: "files" }), serverFixture({ server: "git" })],
      [grantFixture({ server: "files" })],
    );
    const by = Object.fromEntries(rows.map((r) => [r.server, r]));
    expect(rows).toHaveLength(3);
    expect(by.files).toMatchObject({ allowed: true, source: "human", connected: true, widens: false });
    expect(by.git).toMatchObject({ allowed: false, source: null, widens: true, toolCount: 3 });
    expect(by.old).toMatchObject({ allowed: true, connected: false, orphaned: true });
  });

  it("marks rows decided by a wildcard", () => {
    const rows = permissionRows(
      [grantFixture({ label: "*", project: "*", server: "*", source: "explicit" })],
      [serverFixture()],
      null,
    );
    const files = rows.find((r) => r.server === "files")!;
    expect(files).toMatchObject({ allowed: true, viaWildcard: true, source: "explicit" });
    // the wildcard row itself is listed and connected because something matches
    expect(rows.find((r) => r.server === "*")).toMatchObject({ connected: true });
  });
});

describe("spawn helpers (U18)", () => {
  const parent = [grantFixture({ server: "files" }), grantFixture({ server: "git", allowed: false })];
  it("prefills only what the parent may use", () => {
    expect(defaultChildGrants(parent)).toEqual([
      { label: "laptop", project: "", server: "files", allowed: true },
    ]);
  });
  it("refuses grants the parent lacks", () => {
    const refused = refusedGrants(parent, [
      { label: "laptop", server: "files", allowed: true },
      { label: "laptop", server: "git", allowed: true },
      { label: "laptop", server: "shell", allowed: false },
    ]);
    expect(refused.map((r) => r.server)).toEqual(["git"]);
  });
});

describe("permissionRows with a null project", () => {
  it("does not throw and treats null like no project", () => {
    const servers = [
      { label: "l", project: null, server: "b", connected: true, toolCount: 1 },
      { label: "l", project: "", server: "a", connected: true, toolCount: 1 },
    ] as unknown as Parameters<typeof permissionRows>[1];
    const rows = permissionRows([], servers, null);
    expect(rows.map((r) => r.server)).toEqual(["a", "b"]);
  });
});
