package db

import "strings"

// SplitStatements splits a script at top-level semicolons. It understands what
// can hide a ';' in each dialect:
//
//   - '...' strings (” escapes; backslash escapes in MySQL and PostgreSQL E'...')
//   - "..." identifiers/strings and `...` identifiers
//   - -- and /* */ comments (nested in PostgreSQL), # comments in MySQL
//   - PostgreSQL dollar quoting ($$ ... $$, $tag$ ... $tag$)
//   - SQLite CREATE TRIGGER ... BEGIN ...; ...; END
//
// MySQL scripts may use the client command DELIMITER (as mysqldump files do), so
// stored programs with ';' in their body stay in one statement.
// Empty statements and comment-only chunks are dropped; leading comments are
// stripped, except MySQL's executable /*! ... */ comments.
func SplitStatements(kind Kind, src string) []string {
	var out []string
	start, i, n := 0, 0, len(src)

	// SQLite trigger bodies contain ';' before the closing END.
	var words []string
	trigger, begun, ended := false, false, false
	caseDepth := 0
	resetStmt := func() {
		words, trigger, begun, ended, caseDepth = words[:0], false, false, false, 0
	}
	flush := func(end, skip int) {
		if s := strings.TrimSpace(trimLeadingComments(kind, src[start:end])); s != "" {
			out = append(out, s)
		}
		start = end + skip
		resetStmt()
	}
	delim := ";" // changed by the MySQL client command DELIMITER

	escapeNext := false // PostgreSQL E'...' string about to start
	for i < n {
		c := src[i]
		// MySQL client command: "DELIMITER <token>" on its own line, before a statement starts.
		if kind == MySQL && (c == 'D' || c == 'd') && isDelimiterCommand(src, i) &&
			strings.TrimSpace(trimLeadingComments(kind, src[start:i])) == "" {
			eol := skipLine(src, i)
			if tok := strings.TrimSpace(src[i+len("DELIMITER") : eol]); tok != "" {
				delim = tok
			}
			i = eol
			if i < n {
				i++ // the newline
			}
			start = i
			resetStmt()
			continue
		}
		if strings.HasPrefix(src[i:], delim) {
			if trigger && begun && !ended {
				i += len(delim) // still inside the SQLite trigger body
				continue
			}
			flush(i, len(delim))
			i += len(delim)
			continue
		}
		switch {
		case c == '\'':
			backslash := kind == MySQL || escapeNext
			escapeNext = false
			i = skipQuoted(src, i, '\'', backslash)
		case c == '"':
			i = skipQuoted(src, i, '"', kind == MySQL)
		case c == '`':
			i = skipQuoted(src, i, '`', false)
		case c == '-' && i+1 < n && src[i+1] == '-' && lineCommentOK(kind, src, i):
			i = skipLine(src, i)
		case c == '#' && kind == MySQL:
			i = skipLine(src, i)
		case c == '/' && i+1 < n && src[i+1] == '*':
			i = skipBlockComment(src, i, kind == Postgres)
		case c == '$' && kind == Postgres:
			i = skipDollar(src, i)
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(src[j]) {
				j++
			}
			word := strings.ToUpper(src[i:j])
			if len(words) < 3 {
				words = append(words, word)
				if kind == SQLite && len(words) <= 3 {
					trigger = isTriggerStart(words)
				}
			}
			if trigger {
				switch {
				case word == "BEGIN" && !begun:
					begun = true
				case begun && !ended && word == "CASE":
					caseDepth++
				case begun && !ended && word == "END":
					if caseDepth > 0 {
						caseDepth--
					} else {
						ended = true
					}
				}
			}
			escapeNext = kind == Postgres && word == "E" && j < n && src[j] == '\''
			i = j
		default:
			i++
		}
	}
	flush(n, 0)
	return out
}

// isDelimiterCommand reports whether src[i:] starts the word DELIMITER followed by blanks.
func isDelimiterCommand(src string, i int) bool {
	const w = "DELIMITER"
	if len(src)-i <= len(w) || !strings.EqualFold(src[i:i+len(w)], w) {
		return false
	}
	if i > 0 && isIdentChar(src[i-1]) {
		return false
	}
	return src[i+len(w)] == ' ' || src[i+len(w)] == '\t'
}

func isTriggerStart(w []string) bool {
	if len(w) >= 2 && w[0] == "CREATE" && w[1] == "TRIGGER" {
		return true
	}
	return len(w) >= 3 && w[0] == "CREATE" && (w[1] == "TEMP" || w[1] == "TEMPORARY") && w[2] == "TRIGGER"
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$' }

// lineCommentOK: MySQL requires whitespace (or end) after "--".
func lineCommentOK(kind Kind, src string, i int) bool {
	if kind != MySQL {
		return true
	}
	if i+2 >= len(src) {
		return true
	}
	switch src[i+2] {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

func skipLine(src string, i int) int {
	if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(src)
}

// skipQuoted returns the index after the closing quote that starts at i; a
// doubled quote is an escaped quote. An unterminated quote runs to the end.
func skipQuoted(src string, i int, q byte, backslash bool) int {
	j := i + 1
	for j < len(src) {
		switch {
		case backslash && src[j] == '\\':
			j += 2
		case src[j] == q:
			if j+1 < len(src) && src[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return len(src)
}

func skipBlockComment(src string, i int, nested bool) int {
	depth := 1
	j := i + 2
	for j < len(src) {
		switch {
		case nested && strings.HasPrefix(src[j:], "/*"):
			depth++
			j += 2
		case strings.HasPrefix(src[j:], "*/"):
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return len(src)
}

// skipDollar handles PostgreSQL $tag$ ... $tag$ at i; anything else starting
// with '$' (such as the parameter $1) is passed over one byte.
func skipDollar(src string, i int) int {
	if i > 0 && isIdentChar(src[i-1]) {
		return i + 1 // part of an identifier like a$b
	}
	j := i + 1
	if j < len(src) && src[j] >= '0' && src[j] <= '9' {
		return i + 1 // $1 parameter
	}
	for j < len(src) && isIdentChar(src[j]) && src[j] != '$' {
		j++
	}
	if j >= len(src) || src[j] != '$' {
		return i + 1
	}
	tag := src[i : j+1]
	if end := strings.Index(src[j+1:], tag); end >= 0 {
		return j + 1 + end + len(tag)
	}
	return len(src) // unterminated: the rest is one string
}

// trimLeadingComments drops whitespace and comments before the first token so
// the statement's first word can be inspected.
func trimLeadingComments(kind Kind, s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "--") && lineCommentOK(kind, s, 0):
			s = s[skipLine(s, 0):]
		case strings.HasPrefix(s, "#") && kind == MySQL:
			s = s[skipLine(s, 0):]
		case strings.HasPrefix(s, "/*") && !strings.HasPrefix(s, "/*!"):
			s = s[skipBlockComment(s, 0, kind == Postgres):]
		default:
			return s
		}
	}
}
