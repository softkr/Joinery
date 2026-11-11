// SQL editor for dbcat: CodeMirror 6 with per-engine dialects, schema-aware
// completion and colours taken from the app's CSS variables (light/dark).
import { Compartment, EditorState, Prec } from '@codemirror/state';
import { EditorView, drawSelection, highlightActiveLine, keymap, lineNumbers, placeholder } from '@codemirror/view';
import { defaultKeymap, history, historyKeymap, indentWithTab, toggleComment } from '@codemirror/commands';
import { format as formatSql } from 'sql-formatter';
import { MariaSQL, MySQL, PostgreSQL, SQLite, sql } from '@codemirror/lang-sql';
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete';
import { HighlightStyle, bracketMatching, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

const dialects = { sqlite: SQLite, postgres: PostgreSQL, mysql: MySQL, mariadb: MariaSQL };
const formatterLanguage = { sqlite: 'sqlite', postgres: 'postgresql', mysql: 'mysql', mariadb: 'mariadb' };

const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.operatorKeyword, t.modifier], color: 'var(--syn-keyword)', fontWeight: '600' },
  { tag: [t.string, t.special(t.string)], color: 'var(--syn-string)' },
  { tag: [t.number, t.bool, t.null], color: 'var(--syn-number)' },
  { tag: [t.comment, t.lineComment, t.blockComment], color: 'var(--syn-comment)', fontStyle: 'italic' },
  { tag: [t.typeName, t.standard(t.typeName)], color: 'var(--syn-type)' },
  { tag: [t.function(t.variableName), t.standard(t.name)], color: 'var(--syn-func)' },
  { tag: [t.operator, t.punctuation, t.separator], color: 'var(--syn-punct)' },
  { tag: [t.special(t.name)], color: 'var(--syn-ident)' },
]);

const theme = EditorView.theme({
  '&': { height: '100%', color: 'var(--fg)', backgroundColor: 'var(--bg-panel)', fontSize: '13px' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.55', overflow: 'auto' },
  '.cm-content': { caretColor: 'var(--fg)', padding: '8px 0' },
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--fg)' },
  '.cm-gutters': { backgroundColor: 'var(--bg-head)', color: 'var(--fg-dim)', border: 'none', borderRight: '1px solid var(--line-soft)' },
  '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--accent) 7%, transparent)' },
  '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--fg)' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': { backgroundColor: 'var(--accent-soft)' },
  '.cm-matchingBracket': { backgroundColor: 'var(--accent-soft)', outline: '1px solid var(--accent)' },
  '.cm-placeholder': { color: 'var(--fg-dim)' },
  '.cm-tooltip': { backgroundColor: 'var(--bg-panel)', border: '1px solid var(--line)', borderRadius: '8px', boxShadow: 'var(--shadow)', overflow: 'hidden' },
  '.cm-tooltip-autocomplete > ul': { fontFamily: 'var(--mono)', fontSize: '12.5px', maxHeight: '240px' },
  '.cm-tooltip-autocomplete ul li': { padding: '2px 10px' },
  '.cm-tooltip-autocomplete ul li[aria-selected]': { backgroundColor: 'var(--bg-sel)', color: 'var(--fg-sel)' },
  '.cm-completionDetail': { color: 'var(--fg-dim)', marginLeft: '8px', fontStyle: 'normal' },
  '.cm-tooltip-autocomplete ul li[aria-selected] .cm-completionDetail': { color: 'inherit', opacity: '.8' },
});

/**
 * create(parent, opts) -> editor handle
 *   opts.doc         initial text
 *   opts.kind        "sqlite" | "postgres" | "mysql" | "mariadb"
 *   opts.placeholder placeholder text
 *   opts.onChange    (text) => void
 *   opts.onRun       () => void        (Mod-Enter)
 */
function create(parent, opts = {}) {
  const dialect = dialects[opts.kind] || SQLite;
  const language = new Compartment();
  let schema = {};
  const langExt = () => sql({ dialect, schema, upperCaseKeywords: true });

  // Format the selection, or the whole document when nothing is selected. It is
  // a normal edit, so Mod-Z undoes it. Syntax the formatter cannot parse is
  // reported through opts.onError and the text is left untouched.
  const formatDoc = () => {
    const { from, to } = view.state.selection.main;
    const whole = from === to;
    const start = whole ? 0 : from;
    const end = whole ? view.state.doc.length : to;
    const text = view.state.sliceDoc(start, end);
    if (!text.trim()) return false;
    let out;
    try {
      out = formatSql(text, {
        language: formatterLanguage[opts.kind] || 'sql',
        keywordCase: 'upper', tabWidth: 2, linesBetweenQueries: 1,
      }).trimEnd();
    } catch (err) {
      opts.onError?.('포맷할 수 없는 SQL입니다: ' + String(err.message || err).split('\n')[0]);
      return false;
    }
    if (out !== text) view.dispatch({ changes: { from: start, to: end, insert: out }, userEvent: 'input.format' });
    return true;
  };

  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: opts.doc || '',
      extensions: [
        Prec.highest(keymap.of([
          { key: 'Mod-Enter', run: () => { opts.onRun?.(); return true; } },
          { key: 'Alt-Shift-f', run: formatDoc },
          { key: 'Mod-/', run: toggleComment },
        ])),
        lineNumbers(),
        history(),
        drawSelection(),
        highlightActiveLine(),
        bracketMatching(),
        closeBrackets(),
        language.of(langExt()),
        syntaxHighlighting(highlight),
        autocompletion({ activateOnTyping: true, maxRenderedOptions: 60 }),
        keymap.of([...closeBracketsKeymap, ...completionKeymap, ...historyKeymap, indentWithTab, ...defaultKeymap]),
        placeholder(opts.placeholder || ''),
        theme,
        EditorView.contentAttributes.of({ spellcheck: 'false', autocapitalize: 'off', autocorrect: 'off', 'aria-label': 'SQL 에디터' }),
        EditorView.updateListener.of((u) => { if (u.docChanged) opts.onChange?.(u.state.doc.toString()); }),
      ],
    }),
  });

  return {
    view,
    get: () => view.state.doc.toString(),
    set(text) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
    },
    // Replace the selection (or insert at the cursor) with text.
    insert(text) {
      view.dispatch(view.state.replaceSelection(text));
      view.focus();
    },
    selection() {
      const { from, to } = view.state.selection.main;
      return from === to ? '' : view.state.sliceDoc(from, to);
    },
    select(from, to) {
      view.dispatch({ selection: { anchor: from, head: to } });
    },
    // tables: { tableName: [columnName, ...] }
    setSchema(tables) {
      schema = tables || {};
      view.dispatch({ effects: language.reconfigure(langExt()) });
    },
    format: formatDoc,
    toggleComment: () => toggleComment(view),
    focus: () => view.focus(),
    destroy: () => view.destroy(),
  };
}

export { create };
