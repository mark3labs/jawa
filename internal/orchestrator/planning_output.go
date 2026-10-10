package orchestrator

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	planningOutputMaxBytes = 256000
	planningOutputMaxDepth = 32
)

var errPlanningOutput = errors.New("planner must return one unambiguous planning JSON object")

// decodePlanningOutput accepts presentation wrappers and only two syntax repairs:
// trailing commas and unquoted, known report keys. JSON comments are discarded
// only outside strings. No string contents, escapes, or scalar types are repaired.
func decodePlanningOutput(text string) (planningReport, error) {
	var report planningReport
	if len(text) > planningOutputMaxBytes {
		return report, errors.New("planning output exceeds 256000 bytes")
	}
	if !utf8.ValidString(text) {
		return report, errPlanningOutput
	}
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "\ufeff"))
	var candidate []byte
	fenced, seenFence, candidateFenced := false, false, false
	for i := 0; i < len(text); {
		// Fences are recognized only on their own lines, outside JSON values.
		if i == 0 || text[i-1] == '\n' {
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				end = len(text)
			} else {
				end += i
			}
			line := strings.TrimSpace(text[i:end])
			if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
				if fenced {
					if line != "```" || candidate == nil {
						return report, errPlanningOutput
					}
					fenced = false
				} else {
					if seenFence || (line != "```" && !strings.EqualFold(line, "```json")) {
						return report, errPlanningOutput
					}
					fenced, seenFence = true, true
				}
				i = end
				if i < len(text) {
					i++
				}
				continue
			}
		}
		switch text[i] {
		case '{', '[':
			// Every structural candidate counts, including invalid or unrelated
			// objects. Never choose the first/last report from conflicting output.
			if candidate != nil || text[i] != '{' {
				return report, errPlanningOutput
			}
			p := planningJSONParser{text: text, pos: i}
			if err := p.value(1); err != nil {
				return report, err
			}
			candidate, candidateFenced, i = p.out, fenced, p.pos
		case '}', ']':
			return report, errPlanningOutput
		case '"':
			// Quoted prose cannot supply a hidden object candidate.
			if fenced {
				return report, errPlanningOutput
			}
			p := planningJSONParser{text: text, pos: i}
			if _, err := p.stringToken(); err != nil {
				return report, err
			}
			i = p.pos
		default:
			if fenced && !planningJSONSpace(text[i]) {
				return report, errPlanningOutput
			}
			i++
		}
	}
	if candidate == nil || fenced || (seenFence && !candidateFenced) {
		return report, errPlanningOutput
	}
	// RawMessage preserves source types: encoding/json otherwise accepts null
	// for strings and []string elements, and silently accepts duplicate keys.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &fields); err != nil || len(fields) != 3 {
		return report, errPlanningOutput
	}
	for key, dest := range map[string]*string{"spec": &report.Spec, "repo_commit": &report.RepoCommit} {
		raw := fields[key]
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, dest) != nil {
			return report, errPlanningOutput
		}
	}
	raw := fields["questions"]
	if len(raw) == 0 || raw[0] != '[' {
		return report, errors.New("planning report requires a questions array ([] allowed)")
	}
	var questions []json.RawMessage
	if json.Unmarshal(raw, &questions) != nil || len(questions) > 30 {
		return report, errors.New("invalid planning questions array")
	}
	report.Questions = make([]string, len(questions))
	for i, question := range questions {
		if len(question) == 0 || question[0] != '"' || json.Unmarshal(question, &report.Questions[i]) != nil {
			return planningReport{}, errors.New("planning questions must be strings")
		}
	}
	return report, nil
}

// This bounded recursive-descent scanner copies original JSON string tokens.
// It validates all objects (including nested ones) before schema decoding, so
// duplicate keys, malformed candidates and truncation cannot be hidden by repair.
type planningJSONParser struct {
	text string
	pos  int
	out  []byte
}

func planningJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *planningJSONParser) space() error {
	for p.pos < len(p.text) {
		if planningJSONSpace(p.text[p.pos]) {
			p.pos++
			continue
		}
		if strings.HasPrefix(p.text[p.pos:], "//") {
			p.pos += 2
			for p.pos < len(p.text) && p.text[p.pos] != '\n' && p.text[p.pos] != '\r' {
				p.pos++
			}
			continue
		}
		if strings.HasPrefix(p.text[p.pos:], "/*") {
			end := strings.Index(p.text[p.pos+2:], "*/")
			if end < 0 {
				return errPlanningOutput
			}
			p.pos += end + 4
			continue
		}
		break
	}
	return nil
}

func (p *planningJSONParser) stringToken() (string, error) {
	start := p.pos
	p.pos++
	for p.pos < len(p.text) {
		c := p.text[p.pos]
		p.pos++
		switch c {
		case '\\':
			if p.pos == len(p.text) {
				return "", errPlanningOutput
			}
			// encoding/json replaces lone UTF-16 surrogates with U+FFFD.
			// Reject them instead of silently changing source string values.
			if p.text[p.pos] == 'u' {
				code, ok := p.unicodeEscape(p.pos)
				if !ok || (code >= 0xdc00 && code <= 0xdfff) {
					return "", errPlanningOutput
				}
				if code >= 0xd800 && code <= 0xdbff {
					next := p.pos + 5
					if next >= len(p.text) || p.text[next] != '\\' {
						return "", errPlanningOutput
					}
					low, ok := p.unicodeEscape(next + 1)
					if !ok || low < 0xdc00 || low > 0xdfff {
						return "", errPlanningOutput
					}
					p.pos = next + 6
					continue
				}
			}
			p.pos++
		case '"':
			token := p.text[start:p.pos]
			var value string
			if json.Unmarshal([]byte(token), &value) != nil {
				return "", errPlanningOutput
			}
			p.out = append(p.out, token...)
			return value, nil
		}
	}
	return "", errPlanningOutput
}

// pos points to the u in a JSON Unicode escape.
func (p *planningJSONParser) unicodeEscape(pos int) (uint64, bool) {
	if pos+5 > len(p.text) || p.text[pos] != 'u' {
		return 0, false
	}
	code, err := strconv.ParseUint(p.text[pos+1:pos+5], 16, 16)
	return code, err == nil
}

func (p *planningJSONParser) value(depth int) error {
	if depth > planningOutputMaxDepth {
		return errors.New("planning JSON exceeds maximum depth of 32")
	}
	if err := p.space(); err != nil || p.pos == len(p.text) {
		return errPlanningOutput
	}
	switch c := p.text[p.pos]; c {
	case '"':
		_, err := p.stringToken()
		return err
	case '{', '[':
		p.pos++
		p.out = append(p.out, c)
		close := byte(']')
		if c == '{' {
			close = '}'
		}
		keys := make(map[string]bool)
		for {
			if err := p.space(); err != nil || p.pos == len(p.text) {
				return errPlanningOutput
			}
			if p.text[p.pos] == close {
				p.pos++
				p.out = append(p.out, close)
				return nil
			}
			if c == '{' {
				var key string
				if p.text[p.pos] == '"' {
					var err error
					key, err = p.stringToken()
					if err != nil {
						return err
					}
				} else {
					start := p.pos
					for p.pos < len(p.text) && ((p.text[p.pos] >= 'a' && p.text[p.pos] <= 'z') || p.text[p.pos] == '_') {
						p.pos++
					}
					key = p.text[start:p.pos]
					if depth != 1 || (key != "spec" && key != "questions" && key != "repo_commit") {
						return errPlanningOutput
					}
					p.out = append(p.out, '"')
					p.out = append(p.out, key...)
					p.out = append(p.out, '"')
				}
				if keys[key] {
					return errors.New("duplicate planning JSON key")
				}
				keys[key] = true
				if err := p.space(); err != nil || p.pos == len(p.text) || p.text[p.pos] != ':' {
					return errPlanningOutput
				}
				p.pos++
				p.out = append(p.out, ':')
			}
			if err := p.value(depth + 1); err != nil {
				return err
			}
			if err := p.space(); err != nil || p.pos == len(p.text) {
				return errPlanningOutput
			}
			if p.text[p.pos] == close {
				continue
			}
			if p.text[p.pos] != ',' {
				return errPlanningOutput
			}
			p.pos++
			if err := p.space(); err != nil || p.pos == len(p.text) {
				return errPlanningOutput
			}
			// A comma immediately before a close is the only comma repair.
			if p.text[p.pos] != close {
				p.out = append(p.out, ',')
			}
		}
	default:
		start := p.pos
		for p.pos < len(p.text) && !planningJSONSpace(p.text[p.pos]) && !strings.ContainsRune(",]}:/", rune(p.text[p.pos])) {
			p.pos++
		}
		token := p.text[start:p.pos]
		if token == "" || !json.Valid([]byte(token)) {
			return errPlanningOutput
		}
		p.out = append(p.out, token...)
		return nil
	}
}
