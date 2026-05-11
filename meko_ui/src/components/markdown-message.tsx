// Shared markdown renderer for assistant text — used by the Playground chat
// and the Conversations viewer. Centralising it keeps styling consistent
// across surfaces and prevents the "two parallel copies that slowly drift"
// problem we had before this extraction.
//
// `remark-gfm` adds tables / strikethrough / task lists. Long replies skip
// markdown rendering entirely — react-markdown's parse cost grows with
// content length and the LLM rarely needs >150k chars of formatting, so
// the fallback is a plain `<p>` with preserved whitespace.

import {
  Children,
  createElement,
  isValidElement,
  memo,
  type ReactElement,
  type ReactNode,
} from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { cn } from "@/lib/utils";

/** Hard cap on text length we'll hand to react-markdown. Above this we
 *  render a plain `<p>` so a massive paste-in doesn't burn the renderer. */
const MARKDOWN_RENDER_CHARACTER_LIMIT = 150_000;
const MemoizedReactMarkdown = memo(ReactMarkdown);

const isTextElement = (
  child: ReactNode,
): child is ReactElement<{ className?: string }> =>
  isValidElement(child) &&
  typeof child.type === "string" &&
  ["p", "h1", "h2", "h3", "h4", "h5", "h6"].includes(child.type);

// LLMs sometimes wrap list-item content in <p> tags; flatten to inline spans
// so bullets render on the same line as their text instead of breaking.
const transformListItemChildren = (children: ReactNode) =>
  Children.map(children, (child) =>
    isTextElement(child)
      ? createElement("span", {
        ...child.props,
        className: cn(child.props.className, "mb-1"),
      })
      : child,
  );

export function MarkdownMessage({ text }: { text: string }) {
  if (text.length > MARKDOWN_RENDER_CHARACTER_LIMIT) {
    return <p className="whitespace-pre-wrap break-words">{text}</p>;
  }

  return (
    <div className="space-y-2 break-words">
      <MemoizedReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => (
            <p className="mb-2 whitespace-pre-wrap leading-relaxed last:mb-0">
              {children}
            </p>
          ),
          ul: ({ children }) => <ul className="mb-2 ml-5 list-disc">{children}</ul>,
          ol: ({ children }) => (
            <ol className="mb-2 ml-5 list-decimal">{children}</ol>
          ),
          li: ({ children }) => (
            <li className="mt-1 leading-relaxed [&>ol]:pl-4 [&>p]:m-0 [&>p]:inline [&>ul]:pl-4">
              {transformListItemChildren(children)}
            </li>
          ),
          h1: ({ children }) => (
            <h1 className="mb-2 text-base font-semibold">{children}</h1>
          ),
          h2: ({ children }) => (
            <h2 className="mb-2 text-[15px] font-semibold">{children}</h2>
          ),
          h3: ({ children }) => (
            <h3 className="mb-1.5 text-sm font-semibold">{children}</h3>
          ),
          strong: ({ children }) => (
            <strong className="font-semibold text-white">{children}</strong>
          ),
          em: ({ children }) => <em className="italic">{children}</em>,
          hr: () => <hr className="my-3 border-border/60" />,
          code: ({ children }) => (
            <code className="rounded bg-black/25 px-1 py-0.5 font-mono text-[0.9em]">
              {children}
            </code>
          ),
          pre: ({ children }) => (
            <pre className="mb-2 overflow-x-auto rounded-md bg-black/25 p-3 font-mono text-[12px] [&>code]:bg-transparent [&>code]:p-0">
              {children}
            </pre>
          ),
          blockquote: ({ children }) => (
            <blockquote className="mb-2 border-l-2 border-border/70 pl-3 italic text-muted-foreground">
              {children}
            </blockquote>
          ),
          a: ({ children, href }) => (
            <a
              href={href}
              target="_blank"
              rel="noreferrer noopener"
              className="text-blue-300 underline decoration-blue-400/60 underline-offset-2 hover:text-blue-200"
            >
              {children}
            </a>
          ),
          table: ({ children }) => (
            <div className="mb-2 overflow-x-auto">
              <table className="w-full border-collapse text-xs">{children}</table>
            </div>
          ),
          th: ({ children }) => (
            <th className="border border-border/60 bg-black/20 px-2 py-1 text-left font-semibold">
              {children}
            </th>
          ),
          td: ({ children }) => (
            <td className="border border-border/60 px-2 py-1 align-top">
              {children}
            </td>
          ),
        }}
      >
        {text}
      </MemoizedReactMarkdown>
    </div>
  );
}
