import { isValidElement, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import { toJsxRuntime } from "hast-util-to-jsx-runtime";
import { createLowlight } from "lowlight";
import { Fragment, jsx, jsxs } from "react/jsx-runtime";
import remarkGfm from "remark-gfm";
import bash from "highlight.js/lib/languages/bash";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import go from "highlight.js/lib/languages/go";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import markdown from "highlight.js/lib/languages/markdown";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import shell from "highlight.js/lib/languages/shell";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";

// A short list rather than lowlight's "common" set: the syntax definitions are
// most of this chunk, and these cover what an agent usually writes.
const lowlight = createLowlight({ bash, css, diff, go, javascript, json, markdown, python, rust, shell, sql, typescript, xml, yaml, html: xml, js: javascript, ts: typescript, sh: shell, py: python, yml: yaml });

import CopyButton from "../../components/CopyButton";
import "./highlight.css";

/** The plain text under a rendered (highlighted) code element, for copying. */
export function nodeText(node: ReactNode): string {
  if (node === null || node === undefined || typeof node === "boolean") return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(nodeText).join("");
  if (isValidElement(node)) return nodeText((node.props as { children?: ReactNode }).children);
  return "";
}

function languageOf(node: ReactNode): string {
  if (isValidElement(node)) {
    const cls = (node.props as { className?: string }).className ?? "";
    const m = /language-([\w+-]+)/.exec(cls);
    if (m) return m[1];
  }
  return "";
}

/** Assistant text: GitHub-flavoured markdown, highlighted, code copyable. */
export default function Markdown({ text }: { text: string }) {
  return (
    <div className="chat-md break-words text-sm leading-relaxed [&_:not(pre)>code]:rounded [&_:not(pre)>code]:bg-raised [&_:not(pre)>code]:px-1 [&_a]:text-accent [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:border-border [&_blockquote]:pl-3 [&_blockquote]:text-muted [&_h1]:text-lg [&_h1]:font-semibold [&_h2]:text-base [&_h2]:font-semibold [&_h3]:font-semibold [&_li]:my-0.5 [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:my-2 [&_table]:block [&_table]:overflow-x-auto [&_td]:border [&_td]:border-border [&_td]:px-2 [&_td]:py-1 [&_th]:border [&_th]:border-border [&_th]:px-2 [&_th]:py-1 [&_ul]:list-disc [&_ul]:pl-5">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          pre({ children }) {
            const text = nodeText(children).replace(/\n$/, "");
            const lang = languageOf(Array.isArray(children) ? children[0] : children);
            return (
              <div className="my-2 overflow-hidden rounded border border-border bg-raised">
                <div className="flex items-center justify-between border-b border-border px-2 py-0.5 text-xs text-muted">
                  <span className="font-mono">{lang || "code"}</span>
                  <CopyButton value={text} label="Copy code" className="border-0" />
                </div>
                <pre className="overflow-x-auto p-2 font-mono text-xs leading-relaxed">
                  {children}
                </pre>
              </div>
            );
          },
          code({ className, children }) {
            const lang = /language-([\w+-]+)/.exec(className ?? "")?.[1];
            const source = nodeText(children);
            if (lang && lowlight.registered(lang)) {
              try {
                const tree = lowlight.highlight(lang, source);
                return (
                  <code className="hljs font-mono">
                    {toJsxRuntime(tree, { Fragment, jsx, jsxs })}
                  </code>
                );
              } catch {
                // fall through to plain text
              }
            }
            return <code className={`${className ?? ""} font-mono`}>{children}</code>;
          },
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
