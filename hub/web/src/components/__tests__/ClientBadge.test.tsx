import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import ClientBadge from "../ClientBadge";

describe("ClientBadge", () => {
  it("renders every kind as a chip with its human name", () => {
    render(
      <ClientBadge
        label="legion5"
        environment={{
          kinds: ["devcontainer", "container", "direnv", "nix-shell", "venv"],
          project: "myproject",
          workspace: "/work/myproject",
          details: { nixShell: "impure", image: "node:20" },
        }}
      />,
    );
    const badge = screen.getByTestId("client-badge");
    expect(badge).toHaveTextContent("legion5");
    expect(badge).toHaveTextContent("myproject");
    for (const kind of ["devcontainer", "container", "direnv", "venv"]) {
      expect(badge.querySelector(`[data-kind="${kind}"]`)).toHaveTextContent(kind);
    }
    expect(badge.querySelector('[data-kind="nix-shell"]')).toHaveTextContent("nix-shell (impure)");
    expect(badge.getAttribute("title")).toContain("workspace: /work/myproject");
    expect(badge.getAttribute("title")).toContain("image: node:20");
    expect(badge).toHaveAttribute("aria-label", expect.stringContaining("project myproject"));
  });

  it("shows pure nix shells", () => {
    render(<ClientBadge label="a" environment={{ kinds: ["nix-shell"], details: { nixShell: "pure" } }} />);
    expect(screen.getByText("nix-shell (pure)")).toBeInTheDocument();
  });

  it("omits the separator and chips when there is no environment", () => {
    render(<ClientBadge label="old" />);
    const badge = screen.getByTestId("client-badge");
    expect(badge).toHaveTextContent(/^old$/);
    expect(badge.querySelector("[data-kind]")).toBeNull();
    expect(badge).toHaveAttribute("aria-label", "old");
  });

  it("omits the separator when the project is absent", () => {
    render(<ClientBadge label="a" environment={{ kinds: [] }} />);
    expect(screen.queryByText("·")).toBeNull();
  });

  it("truncates long names instead of overflowing", () => {
    render(<ClientBadge label={"x".repeat(200)} environment={{ kinds: ["direnv"], project: "p".repeat(200) }} />);
    const badge = screen.getByTestId("client-badge");
    expect(badge.className).toContain("max-w-full");
    expect(screen.getByText("x".repeat(200)).className).toContain("truncate");
    expect(screen.getByText("p".repeat(200)).className).toContain("truncate");
  });

  it("wrap: never truncates the label, even a long one, and fills the width instead of shrink-to-fit", () => {
    const long = "agent-reverse-proxy-workspace-dev-box";
    render(<ClientBadge label={long} environment={{ kinds: ["direnv"], project: "p".repeat(200) }} wrap />);
    const badge = screen.getByTestId("client-badge");
    expect(badge.className).toContain("w-full");
    expect(badge.className).not.toContain("inline-flex");
    const labelEl = screen.getByText(long);
    expect(labelEl.className).not.toContain("truncate");
    expect(labelEl.textContent).toBe(long);
    // the project may still ellipsize; the label never does.
    expect(screen.getByText("p".repeat(200)).className).toContain("truncate");
  });
});
