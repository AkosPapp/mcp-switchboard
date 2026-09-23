import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import Markdown, { nestFences, normalizeMath } from "../Markdown";

describe("normalizeMath", () => {
  it("rewrites \\( \\) and \\[ \\] to dollar forms", () => {
    expect(normalizeMath("a \\(x^2\\) b")).toBe("a $x^2$ b");
    expect(normalizeMath("\\[E=mc^2\\]")).toContain("$$\nE=mc^2\n$$");
  });
  it("leaves code alone", () => {
    expect(normalizeMath("`\\(x\\)`")).toBe("`\\(x\\)`");
    expect(normalizeMath("```\n\\(x\\)\n```")).toBe("```\n\\(x\\)\n```");
  });
});

describe("Markdown", () => {
  it("renders inline and display maths with KaTeX", () => {
    const { container } = render(<Markdown text={"inline $a^2$ and\n\n$$\n\\int_0^1 x\\,dx\n$$"} />);
    expect(container.querySelectorAll(".katex").length).toBe(2);
    expect(container.querySelector(".katex-display")).not.toBeNull();
  });
  it("keeps a mermaid block readable until the diagram renders", () => {
    const { container } = render(<Markdown text={"```mermaid\ngraph TD; A-->B\n```"} />);
    expect(container.textContent).toContain("graph TD; A-->B");
  });
});

describe("nested fences", () => {
  const example = [
    "```markdown",
    "this block is not formatted",
    "```mermaid",
    "graph TD",
    "    A[Me] -->|connected| B[Agent 1]",
    "```",
    "still inside the outer block",
    "```",
    "here is the end",
  ].join("\n");

  it("lengthens the outer fence past the inner ones", () => {
    const out = nestFences(example).split("\n");
    expect(out[0]).toBe("````markdown");
    expect(out[7]).toBe("````");
    expect(out[2]).toBe("```mermaid"); // inner fences are untouched
  });

  it("renders one code block holding the inner fences, and the prose after it as prose", () => {
    const { container } = render(<Markdown text={example} />);
    const blocks = container.querySelectorAll("pre");
    expect(blocks).toHaveLength(1);
    expect(blocks[0].textContent).toContain("```mermaid");
    expect(blocks[0].textContent).toContain("still inside the outer block");
    expect(blocks[0].textContent).not.toContain("here is the end");
    expect(container.textContent).toContain("here is the end");
    expect(container.querySelector('[data-testid="mermaid"]')).toBeNull(); // shown as source, not drawn
  });

  it("leaves ordinary and already-longer fences alone", () => {
    const plain = "```js\nlet a;\n```\ntext\n```py\nx\n```";
    expect(nestFences(plain)).toBe(plain);
    const longer = "````markdown\n```js\nx\n```\n````";
    expect(nestFences(longer)).toBe(longer);
    const unclosed = "```markdown\n```js\nx";
    expect(nestFences(unclosed)).toBe(unclosed);
  });
});
