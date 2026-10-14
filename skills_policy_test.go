package main

import (
	"strings"
	"testing"
)

func TestEmbeddedDeliveryReviewPolicy(t *testing.T) {
	texts := map[string]string{"instructions.md": _instructions}
	for _, name := range []string{"deliver-pr", "resolve-reviews"} {
		path := "skills/" + name + "/SKILL.md"
		data, err := _skills.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		texts[path] = string(data)
	}
	for path, text := range texts {
		t.Run(path, func(t *testing.T) {
			// Normalize Markdown wrapping, not meaning; these are shared contracts,
			// not snapshots of the surrounding prose or a ban on bot names.
			text = strings.Join(strings.Fields(text), " ")
			for _, phrase := range []string{
				"inspect current-head checks",
				"all-author feedback",
				"wait for observed pending work",
				"Do not wait for or request a nonexistent review",
				"repository/provider no-CI policy",
			} {
				if !strings.Contains(strings.ToLower(text), strings.ToLower(phrase)) {
					t.Errorf("missing delivery contract %q", phrase)
				}
			}
			for _, obsolete := range []string{
				"wait only for the initial review",
				"do not wait for additional reviews",
				"repair tasks do not wait for reviews",
				"scope to bots unless",
				"request exactly one",
				"request one review",
				"about 10 minutes",
				"however long the posted wait is",
				"reviews each pr once",
				"review bots skip it",
			} {
				if strings.Contains(strings.ToLower(text), obsolete) {
					t.Errorf("obsolete scheduling directive %q", obsolete)
				}
			}
		})
	}
}
