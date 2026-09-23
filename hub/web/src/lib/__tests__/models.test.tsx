import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { ModelInfo } from "../../api/types";
import { BudgetMeter, TruncationWarning } from "../../views/chat/Composer";
import { formatContext, lacksTools, ModelOptions, modelOptionLabel, NoToolsWarning } from "../models";

const models: ModelInfo[] = [
  { provider: "fake", model: "echo" },
  { provider: "ollama", model: "gpt-oss:20b", contextWindow: 32768, supportsTools: true, discovered: true },
  { provider: "ollama", model: "tiny", contextWindow: 131072, supportsTools: false, discovered: true },
];

describe("formatContext", () => {
  it("abbreviates", () => {
    expect(formatContext(512)).toBe("512");
    expect(formatContext(32768)).toBe("32k");
    expect(formatContext(131072)).toBe("128k");
    expect(formatContext(1048576)).toBe("1M");
    expect(formatContext(1572864)).toBe("1.5M");
  });
});

describe("modelOptionLabel", () => {
  it("adds context and the tools marker only when known", () => {
    expect(modelOptionLabel(models[0])).toBe("fake/echo");
    expect(modelOptionLabel(models[1])).toBe("ollama/gpt-oss:20b · 32k ctx");
    expect(modelOptionLabel(models[2])).toBe("ollama/tiny · 128k ctx · no tool support");
  });
  it("lacksTools is only true for an explicit false", () => {
    expect(lacksTools(models, "ollama/tiny")).toBe(true);
    expect(lacksTools(models, "fake/echo")).toBe(false);
    expect(lacksTools(models, "missing/x")).toBe(false);
  });
});

describe("model picker", () => {
  it("groups discovered models and warns on no-tool ones", () => {
    const { rerender } = render(
      <>
        <select aria-label="m">
          <ModelOptions models={models} />
        </select>
        <NoToolsWarning models={models} value="ollama/tiny" />
      </>,
    );
    expect(screen.getByRole("group", { name: "Discovered" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "ollama/gpt-oss:20b · 32k ctx" })).toBeTruthy();
    expect(screen.getByText("this model does not advertise tool calling")).toBeTruthy();
    rerender(<NoToolsWarning models={models} value="fake/echo" />);
    expect(screen.queryByTestId("no-tools-warning")).toBeNull();
  });
});

describe("TruncationWarning", () => {
  it("shows with the window size when truncated", () => {
    render(<TruncationWarning usage={{ truncated: true, contextWindow: 32768 }} />);
    expect(screen.getByRole("alert").textContent).toContain("(32,768 tokens)");
    expect(screen.getByRole("alert").textContent).toContain("give this chat fewer tools");
  });
  it("shows without numbers, and hides otherwise", () => {
    const { rerender } = render(<TruncationWarning usage={{ truncated: true }} />);
    expect(screen.getByRole("alert")).toBeTruthy();
    rerender(<TruncationWarning usage={{ tokens: 5 }} />);
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it("the budget meter tolerates usage without the new fields", () => {
    render(<BudgetMeter budget={{ max_tokens: 100 }} usage={{}} />);
    expect(screen.getByTestId("budget-meter")).toBeTruthy();
  });
});
