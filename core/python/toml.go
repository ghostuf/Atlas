package python

// A narrow, line-oriented TOML-subset scanner — not a general TOML
// implementation — scoped to exactly what poetry.lock/pyproject.toml use:
// [[array-of-table]] / [dotted.table] headers, and key = value pairs whose
// VALUE we almost never need to interpret (a dependency's value might be a
// plain version string or an inline table like {version="...",
// optional=true} — for graph edges only the key, the dependency's name,
// matters). So most of the work here is correctly finding where a value
// *ends* — tracking bracket/brace depth and quote state — not parsing its
// contents. Unrecognized sections ([package.extras], [metadata.files], ...)
// need no special-case skip logic: they just produce a TOMLSection nothing
// downstream looks at.
//
// Known, accepted gaps (fine for poetry.lock/pyproject.toml in practice):
// no multi-line basic/literal strings, no dotted keys outside headers, no
// quote-awareness when splitting a header path on '.'.

import (
	"strings"
)

// TOMLSection is one [header] or [[header]] block, in file order.
type TOMLSection struct {
	Path      []string // e.g. ["tool","poetry","group","dev","dependencies"]
	IsArray   bool     // true for [[package]], false for [package.dependencies]
	StartLine int      // 0-based index of the "[...]" line itself
	Pairs     []TOMLPair
}

// TOMLPair is one key = value line (or the start of one) within a section.
type TOMLPair struct {
	Key string // bare or quoted key, quotes stripped
	Raw string // raw, trimmed value text — may have come from multiple lines
}

// ParseTOMLSections scans raw TOML bytes top to bottom and returns every
// section it finds, each carrying its own key/value pairs.
func ParseTOMLSections(data []byte) ([]TOMLSection, error) {
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}

	var sections []TOMLSection
	var cur *TOMLSection

	// value-assembly state, carried across lines when a value spans more than one
	var assembling bool
	var pendingKey string
	var pendingRaw strings.Builder
	var depth int // bracket/brace nesting of the value currently being assembled

	finishPair := func() {
		if cur != nil {
			cur.Pairs = append(cur.Pairs, TOMLPair{Key: pendingKey, Raw: strings.TrimSpace(pendingRaw.String())})
		}
		pendingKey = ""
		pendingRaw.Reset()
		assembling = false
		depth = 0
	}

	// scanValue consumes as much of `s` as the current value needs, appending
	// to pendingRaw and updating depth/assembling. Returns true once the value
	// is fully closed (depth back to 0, outside any quote) on this line.
	scanValue := func(s string) bool {
		inStr := false
		var quote byte
		for i := 0; i < len(s); i++ {
			c := s[i]
			if inStr {
				pendingRaw.WriteByte(c)
				if c == '\\' && quote == '"' && i+1 < len(s) {
					i++
					pendingRaw.WriteByte(s[i])
					continue
				}
				if c == quote {
					inStr = false
				}
				continue
			}
			switch c {
			case '"', '\'':
				inStr = true
				quote = c
				pendingRaw.WriteByte(c)
			case '[', '{':
				depth++
				pendingRaw.WriteByte(c)
			case ']', '}':
				depth--
				pendingRaw.WriteByte(c)
			case '#':
				// a bare '#' outside a string, at depth 0, ends the value (comment)
				if depth <= 0 {
					return true
				}
				pendingRaw.WriteByte(c)
			default:
				pendingRaw.WriteByte(c)
			}
		}
		return depth <= 0 && !inStr
	}

	for lineNo, line := range lines {
		if assembling {
			if scanValue(line) {
				finishPair()
			} else {
				pendingRaw.WriteByte('\n')
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "[") {
			isArray := strings.HasPrefix(trimmed, "[[")
			inner := trimmed
			if isArray {
				inner = strings.TrimSuffix(strings.TrimPrefix(inner, "[["), "]]")
			} else {
				inner = strings.TrimSuffix(strings.TrimPrefix(inner, "["), "]")
			}
			var path []string
			for _, seg := range strings.Split(inner, ".") {
				seg = strings.TrimSpace(seg)
				seg = strings.Trim(seg, `"'`)
				path = append(path, seg)
			}
			if cur != nil {
				sections = append(sections, *cur)
			}
			cur = &TOMLSection{Path: path, IsArray: isArray, StartLine: lineNo}
			continue
		}

		// key = value
		key, rest, ok := splitKey(trimmed)
		if !ok {
			continue // malformed/unsupported line in our narrow subset — skip it
		}
		pendingKey = key
		pendingRaw.Reset()
		depth = 0
		if scanValue(rest) {
			finishPair()
		} else {
			assembling = true
			pendingRaw.WriteByte('\n')
		}
	}
	if assembling {
		finishPair() // unterminated value at EOF — best effort, don't error
	}
	if cur != nil {
		sections = append(sections, *cur)
	}
	return sections, nil
}

// splitKey parses a bare or quoted key off the front of a trimmed line,
// returning the key, the remainder after '=', and whether a key was found.
func splitKey(line string) (key, rest string, ok bool) {
	if line == "" {
		return "", "", false
	}
	if line[0] == '"' || line[0] == '\'' {
		quote := line[0]
		end := strings.IndexByte(line[1:], quote)
		if end < 0 {
			return "", "", false
		}
		key = line[1 : 1+end]
		line = line[1+end+1:]
	} else {
		i := 0
		for i < len(line) && line[i] != '=' && !isSpaceByte(line[i]) {
			i++
		}
		if i == 0 {
			return "", "", false
		}
		key = line[:i]
		line = line[i:]
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	return key, line[eq+1:], true
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' }

// TOMLString reads a raw value as a quoted string, unquoting it.
func TOMLString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return "", false
	}
	q := raw[0]
	if (q != '"' && q != '\'') || raw[len(raw)-1] != q {
		return "", false
	}
	inner := raw[1 : len(raw)-1]
	if q == '"' {
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
	}
	return inner, true
}

// TOMLBool reads a raw value as a bare boolean literal.
func TOMLBool(raw string) (bool, bool) {
	switch strings.TrimSpace(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// TOMLStringArray reads a raw value as a simple array of quoted strings,
// e.g. ["main", "dev"]. Only handles flat string arrays (what groups/tags
// actually are in poetry.lock) — not nested arrays or inline tables.
func TOMLStringArray(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, false
	}
	inner := raw[1 : len(raw)-1]
	var out []string
	var cur strings.Builder
	inStr := false
	var quote byte
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			if v, ok := TOMLString(s); ok {
				out = append(out, v)
			}
		}
		cur.Reset()
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inStr {
			cur.WriteByte(c)
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
			cur.WriteByte(c)
		case ',':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out, true
}
