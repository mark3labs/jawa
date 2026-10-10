package orchestrator

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func planningParserFixture(t *testing.T) (planningReport, string) {
	t.Helper()
	r := planningReport{
		Spec:       "Plan 🚀\n\n```json\n{\"questions\":null,\"spec\":\"fake\"}\n```\nBraces: {} []; path C:\\work\\repo; quote \"hello\"; /* not a comment */ // keep\nEscapes: literal \\n and \\u0041",
		Questions:  []string{"Which store?", "Keep `}` and ``` fences?"},
		RepoCommit: strings.Repeat("a", 40),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, string(b)
}

func TestPlanningOutputPresentationAndRepairs(t *testing.T) {
	want, raw := planningParserFixture(t)
	bareKeys := strings.NewReplacer(`"spec":`, `spec:`, `"questions":`, `questions:`, `"repo_commit":`, `repo_commit:`).Replace(raw)
	for name, text := range map[string]string{
		"bare":                 raw,
		"bom":                  "\ufeff\r\n" + raw,
		"heading":              "# Planning report\nHere is my report:\n" + raw + "\nReady for review.",
		"prose fence":          "Here is the report:\n```JsOn\r\n" + raw + "\r\n```\r\nDone.",
		"unlabelled":           "Summary:\n  ```\n" + raw + "\n  ```\nEnd.",
		"bare keys":            bareKeys,
		"trailing comma":       raw[:len(raw)-1] + ",}",
		"array trailing comma": strings.Replace(raw, `],"repo_commit"`, `,],"repo_commit"`, 1),
		"comments":             strings.Replace(raw, `"questions":`, "/* braces { and quotes \" */\n\"questions\": // question list\r\n", 1),
		"combined":             strings.Replace(bareKeys[:len(bareKeys)-1], "questions:", "/* notes */ questions:", 1) + ", /* done */ }",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePlanningReport(text)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("report changed or rejected: %v", err)
			}
		})
	}
}

func TestPlanningOutputFailClosed(t *testing.T) {
	_, raw := planningParserFixture(t)
	sha := strings.Repeat("a", 40)
	for name, text := range map[string]string{
		"multiple identical":       raw + "\n" + raw,
		"multiple conflicting":     raw + ` {"spec":"different"}`,
		"unrelated before":         "{}\n" + raw,
		"broken before":            "{broken}\n" + raw,
		"nested wrapper":           `{"report":` + raw + `}`,
		"array wrapper":            "[" + raw + "]",
		"extra closing brace":      raw + "}",
		"multiple fences":          "```json\n" + raw + "\n```\n```json\n" + raw + "\n```",
		"wrong fence":              "```js\n" + raw + "\n```",
		"wrong close":              "```json\n" + raw + "\n```json",
		"missing close":            "```json\n" + raw,
		"empty fence before":       "```\n```\n" + raw,
		"fence prose":              "```json\nHere is JSON:\n" + raw + "\n```",
		"missing questions":        `{"spec":"s","repo_commit":"` + sha + `"}`,
		"null questions":           `{"spec":"s","repo_commit":"` + sha + `","questions":null}`,
		"scalar questions":         `{"spec":"s","repo_commit":"` + sha + `","questions":"q"}`,
		"null element":             `{"spec":"s","repo_commit":"` + sha + `","questions":[null]}`,
		"numeric element":          `{"spec":"s","repo_commit":"` + sha + `","questions":[12]}`,
		"boolean element":          `{"spec":"s","repo_commit":"` + sha + `","questions":[true]}`,
		"object element":           `{"spec":"s","repo_commit":"` + sha + `","questions":[{}]}`,
		"null spec":                strings.Replace(raw, `"spec":`, `"spec":null,"unused":`, 1),
		"numeric spec":             `{"spec":1,"repo_commit":"` + sha + `","questions":[]}`,
		"null commit":              `{"spec":"s","repo_commit":null,"questions":[]}`,
		"unknown":                  raw[:len(raw)-1] + `,"extra":1}`,
		"case alias":               strings.Replace(raw, `"spec":`, `"Spec":`, 1),
		"duplicate":                raw[:len(raw)-1] + `,"questions":[]}`,
		"escaped duplicate":        raw[:len(raw)-1] + `,"\u0073pec":"duplicate"}`,
		"bare duplicate":           raw[:len(raw)-1] + `,spec:"duplicate"}`,
		"nested duplicate":         raw[:len(raw)-1] + `,"extra":{"x":1,"x":2}}`,
		"nested escaped duplicate": raw[:len(raw)-1] + `,"extra":[{"x":1,"\u0078":2}]}`,
		"unknown bare key":         raw[:len(raw)-1] + `,extra:1}`,
		"lone high surrogate":      strings.Replace(raw, `Plan`, `\ud800`, 1),
		"lone low surrogate":       strings.Replace(raw, `Plan`, `\udc00`, 1),
		"mismatched surrogate":     strings.Replace(raw, `Plan`, `\ud800\u0041`, 1),
		"bad escape":               strings.Replace(raw, `Plan`, `\w`, 1),
		"literal newline":          strings.Replace(raw, `Plan`, "Plan\n", 1),
		"single quotes":            `{'spec':'s','questions':[],'repo_commit':'` + sha + `'}`,
		"double comma":             raw[:len(raw)-1] + `,,}`,
		"missing comma":            strings.Replace(raw, `,"questions"`, ` "questions"`, 1),
		"truncated comment":        raw[:len(raw)-1] + `/*`,
		"invalid utf8":             raw + string([]byte{0xff}),
		"oversize":                 strings.Repeat(" ", planningOutputMaxBytes) + raw,
		"deep":                     `{"spec":` + strings.Repeat("[", planningOutputMaxDepth+1) + `0` + strings.Repeat("]", planningOutputMaxDepth+1) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePlanningReport(text); err == nil {
				t.Fatal("accepted ambiguous or malformed report")
			}
		})
	}
	// Every byte truncation of a bare object must fail, even after all fields
	// have appeared: repairs must never fabricate a closing bracket or string.
	for i := 0; i < len(raw); i++ {
		if _, err := parsePlanningReport(raw[:i]); err == nil {
			t.Fatalf("accepted truncation at %d", i)
		}
	}
}

func TestPlanningOutputNormalizationAndBounds(t *testing.T) {
	r := planningReport{Spec: "  spec\n ", Questions: []string{" q "}, RepoCommit: " " + strings.Repeat("B", 64) + " "}
	b, _ := json.Marshal(r)
	got, err := parsePlanningReport(string(b))
	if err != nil || got.Spec != "spec" || got.Questions[0] != "q" || got.RepoCommit != strings.Repeat("b", 64) {
		t.Fatal("existing normalization changed", err)
	}
	for _, r := range []planningReport{
		{Spec: " ", Questions: []string{}, RepoCommit: strings.Repeat("a", 40)},
		{Spec: "s", Questions: []string{" "}, RepoCommit: strings.Repeat("a", 40)},
		{Spec: "s", Questions: []string{strings.Repeat("q", 4001)}, RepoCommit: strings.Repeat("a", 40)},
		{Spec: "s", Questions: make([]string, 31), RepoCommit: strings.Repeat("a", 40)},
		{Spec: "s", Questions: []string{}, RepoCommit: "abc"},
	} {
		b, _ := json.Marshal(r)
		if _, err := parsePlanningReport(string(b)); err == nil {
			t.Fatal("accepted invalid normalized report")
		}
	}
	// Exact size boundary, including wrapper prose.
	r = planningReport{Spec: "s", Questions: []string{}, RepoCommit: strings.Repeat("a", 40)}
	b, _ = json.Marshal(r)
	if _, err := parsePlanningReport(strings.Repeat(" ", planningOutputMaxBytes-len(b)) + string(b)); err != nil {
		t.Fatal("rejected size boundary", err)
	}
}

// Opt-in private regression: feed the stored response via an environment
// variable; neither private values nor a fixture are persisted or logged.
// The independent baseline uses strict JSON decoding of its single fence.
func TestPlanningOutputSavedResponse(t *testing.T) {
	text := os.Getenv("JAWA_PLANNING_SAVED_RESPONSE")
	if text == "" {
		t.Skip("private saved response not supplied")
	}
	start := strings.Index(text, "```json\n")
	end := strings.LastIndex(text, "\n```")
	if start < 0 || end <= start {
		t.Fatal("saved response fence not found")
	}
	var want planningReport
	if json.Unmarshal([]byte(text[start+len("```json\n"):end]), &want) != nil {
		t.Fatal("independent strict baseline failed")
	}
	decoded, err := decodePlanningOutput(text)
	if err != nil || !reflect.DeepEqual(decoded, want) {
		t.Fatal("saved response extraction did not preserve exact field values")
	}
	// Existing normalization intentionally trims outer whitespace only.
	want.Spec = strings.TrimSpace(want.Spec)
	want.RepoCommit = strings.ToLower(strings.TrimSpace(want.RepoCommit))
	for i := range want.Questions {
		want.Questions[i] = strings.TrimSpace(want.Questions[i])
	}
	parsed, err := parsePlanningReport(text)
	if err != nil || !reflect.DeepEqual(parsed, want) {
		t.Fatal("saved response normalization did not preserve exact field values")
	}
}

func FuzzPlanningOutput(f *testing.F) {
	for _, seed := range []string{
		`{"spec":"s","questions":[],"repo_commit":"` + strings.Repeat("a", 40) + `"}`,
		"Here is JSON:\n```JSON\n{spec:\"s\", questions:[], repo_commit:\"" + strings.Repeat("a", 40) + "\",}\n```\nDone.",
		`{"spec":"fake { } \\"","questions":[null]}`,
		`{"spec":{"x":1,"x":2}}`, "\ufeff", "/*", "```", "{", "[]",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		r, err := parsePlanningReport(text)
		if err != nil {
			return
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal("accepted report cannot be serialized")
		}
		roundTrip, err := parsePlanningReport(string(b))
		if err != nil || !reflect.DeepEqual(roundTrip, r) {
			t.Fatal("accepted report is not stable")
		}
	})
}

func TestPlanningOutputUnicodeSurrogatePair(t *testing.T) {
	_, raw := planningParserFixture(t)
	got, err := parsePlanningReport(strings.Replace(raw, "🚀", `\ud83d\ude80`, 1))
	want, _ := planningParserFixture(t)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("valid surrogate pair was not preserved", err)
	}
}

func TestPlanningJSONScannerDuplicatesAndDepth(t *testing.T) {
	for _, text := range []string{
		`{"outer":{"x":1,"x":2}}`,
		`{"outer":[{"x":1,"\u0078":2}]}`,
		`{"spec":"one",spec:"two"}`,
	} {
		p := planningJSONParser{text: text}
		if err := p.value(1); err == nil || err.Error() != "duplicate planning JSON key" {
			t.Fatal("scanner did not detect decoded duplicate keys", err)
		}
	}
	for _, depth := range []int{planningOutputMaxDepth - 1, planningOutputMaxDepth} {
		text := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
		p := planningJSONParser{text: text}
		err := p.value(1)
		if (err == nil) != (depth < planningOutputMaxDepth) {
			t.Fatalf("incorrect depth boundary at %d", depth)
		}
	}
}
