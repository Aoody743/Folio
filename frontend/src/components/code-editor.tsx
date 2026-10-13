"use client";
import { useEffect, useRef } from "react";
import { EditorView, basicSetup } from "codemirror";
import { Compartment, EditorState, Extension } from "@codemirror/state";
import { json } from "@codemirror/lang-json";
import { javascript } from "@codemirror/lang-javascript";
import { html } from "@codemirror/lang-html";
import { css } from "@codemirror/lang-css";
import { sql } from "@codemirror/lang-sql";
import { markdown } from "@codemirror/lang-markdown";
const syntax = (filename: string): Extension =>
  /\.json$/i.test(filename)
    ? json()
    : /\.[jt]sx?$/i.test(filename)
      ? javascript({
          typescript: /\.tsx?$/i.test(filename),
          jsx: /\.[jt]sx$/i.test(filename),
        })
      : /\.html?$/i.test(filename)
        ? html()
        : /\.css$/i.test(filename)
          ? css()
          : /\.sql$/i.test(filename)
            ? sql()
            : /\.md$/i.test(filename)
              ? markdown()
              : [];
export default function CodeEditor({
  value,
  onChange,
  filename,
  readOnly = false,
}: {
  value: string;
  onChange: (value: string) => void;
  filename: string;
  readOnly?: boolean;
}) {
  const root = useRef<HTMLDivElement>(null);
  const editor = useRef<EditorView | undefined>(undefined);
  const listener = useRef(onChange);
  listener.current = onChange;
  const readonly = useRef(new Compartment());
  useEffect(() => {
    if (!root.current) return;
    const view = new EditorView({
      doc: value,
      parent: root.current,
      extensions: [
        basicSetup,
        syntax(filename),
        EditorView.lineWrapping,
        readonly.current.of([
          EditorState.readOnly.of(readOnly),
          EditorView.editable.of(!readOnly),
        ]),
        EditorView.contentAttributes.of({
          "aria-label": "文件内容",
          role: "textbox",
          "aria-multiline": "true",
        }),
        // Native contenteditable insertion builds thousands of line nodes
        // before CodeMirror can read the mutation, blocking Chromium. Apply
        // bulk plain-text input as one editor transaction instead.
        EditorView.domEventHandlers({
          beforeinput(event, view) {
            const text = event.data;
            if (
              event.inputType !== "insertText" ||
              !text ||
              text.length < 1024 ||
              !event.cancelable ||
              event.isComposing ||
              view.state.readOnly
            )
              return false;
            let { from, to } = view.state.selection.main;
            const selection = view.dom.ownerDocument.getSelection();
            if (selection?.rangeCount) {
              const range = selection.getRangeAt(0);
              if (
                view.contentDOM.contains(range.startContainer) &&
                view.contentDOM.contains(range.endContainer)
              ) {
                if (
                  range.startContainer === view.contentDOM &&
                  range.endContainer === view.contentDOM &&
                  range.startOffset === 0 &&
                  range.endOffset === view.contentDOM.childNodes.length
                ) {
                  from = 0;
                  to = view.state.doc.length;
                } else {
                  from = view.posAtDOM(range.startContainer, range.startOffset);
                  to = view.posAtDOM(range.endContainer, range.endOffset);
                }
              }
            }
            event.preventDefault();
            const main = view.state.selection.main;
            view.dispatch({
              ...(from === main.from && to === main.to
                ? view.state.replaceSelection(text)
                : {
                    changes: { from, to, insert: text },
                    selection: { anchor: from + text.length },
                  }),
              userEvent: "input.type",
              scrollIntoView: true,
            });
            return true;
          },
        }),
        EditorView.theme({
          "&": {
            backgroundColor: "var(--bg)",
            color: "var(--text)",
            minHeight: "360px",
            borderRadius: "12px",
          },
          ".cm-content": {
            fontFamily: "ui-monospace, monospace",
            fontSize: "13px",
            padding: "12px 0",
          },
          ".cm-scroller": { overflow: "auto", maxHeight: "55dvh" },
          ".cm-gutters": {
            backgroundColor: "var(--surface)",
            color: "var(--muted)",
            borderRight: "1px solid var(--border)",
          },
          "&.cm-focused": { outline: "2px solid var(--accent)" },
          ".cm-activeLine": {
            backgroundColor: "color-mix(in srgb,var(--accent) 5%,transparent)",
          },
          ".cm-activeLineGutter": { backgroundColor: "var(--soft)" },
          ".cm-cursor": { borderLeftColor: "var(--text)" },
        }),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) listener.current(update.state.doc.toString());
        }),
      ],
    });
    editor.current = view;
    return () => {
      view.destroy();
      editor.current = undefined;
    };
  }, [filename]);
  useEffect(() => {
    const view = editor.current;
    if (view && view.state.doc.toString() !== value)
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: value },
      });
  }, [value]);
  useEffect(() => {
    editor.current?.dispatch({
      effects: readonly.current.reconfigure([
        EditorState.readOnly.of(readOnly),
        EditorView.editable.of(!readOnly),
      ]),
    });
  }, [readOnly]);
  return <div ref={root} className="code-editor" />;
}
