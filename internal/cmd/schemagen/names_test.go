package main

import "testing"

// The schema spells names three ways — PascalCase definitions, camelCase
// properties, snake_case union tags — and Go's convention capitalises some words
// whole. Between them that is the whole of the naming rule, and it decides
// published identifiers, so it is stated rather than left to be read off the
// generated file.
func TestGoNamesFollowTheInitialismRule(t *testing.T) {
	tests := []struct {
		name string
		want string
		why  string
	}{
		{name: "SessionId", want: "SessionID", why: "a definition whose last word is an initialism"},
		{name: "sessionId", want: "SessionID", why: "the same word as a property"},
		{name: "HttpHeader", want: "HTTPHeader", why: "an initialism at the start"},
		{name: "McpServerHttp", want: "McpServerHTTP", why: "and at the end"},
		{name: "McpServerAcpId", want: "McpServerAcpID", why: "ACP is not on Go's list, so it is left alone"},
		{name: "LlmProtocol", want: "LlmProtocol", why: "nor is LLM"},
		{name: "ElicitationUrlMode", want: "ElicitationURLMode", why: "an initialism in the middle"},
		{name: "mimeType", want: "MimeType", why: "MIME is not on the list either"},
		{name: "_meta", want: "Meta", why: "a leading underscore is a separator, not a word"},
		{name: "user_message_chunk", want: "UserMessageChunk", why: "a union tag"},
		{name: "resource_link", want: "ResourceLink", why: "and another"},
		{name: "anyOf", want: "AnyOf", why: "a property named after a schema keyword is still a property"},
		{name: "cwd", want: "Cwd", why: "a word that looks like an initialism but is not on the list"},
		{name: "url", want: "URL", why: "one that is"},
	}

	for _, test := range tests {
		if got := goName(test.name); got != test.want {
			t.Errorf("goName(%q) = %q, want %q (%s)", test.name, got, test.want, test.why)
		}
	}
}

// Arm names are allocated over the whole schema so that growing the manifest
// cannot rename a type that was already published. The collision rule is what
// makes that work, and it fires for real: SessionUpdate's plan_update arm wants
// the name of the payload it wraps.
func TestArmNamesAreQualifiedOnlyWhenTaken(t *testing.T) {
	names := newNames()
	if err := names.claim("PlanUpdate", "#/$defs/PlanUpdate"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	free, err := names.claimArm("UserMessageChunk", "SessionUpdate", "arm")
	if err != nil {
		t.Fatalf("claimArm: %v", err)
	}
	if free != "UserMessageChunk" {
		t.Errorf("an unclaimed arm name became %q", free)
	}

	taken, err := names.claimArm("PlanUpdate", "SessionUpdate", "arm")
	if err != nil {
		t.Fatalf("claimArm: %v", err)
	}
	if taken != "SessionUpdatePlanUpdate" {
		t.Errorf("a taken arm name became %q, want SessionUpdatePlanUpdate", taken)
	}

	// Two constructs wanting one name is a generator bug or a schema change, and
	// either way it has to stop generation rather than pick a winner.
	if err := names.claim("PlanUpdate", "#/$defs/Something"); err == nil {
		t.Error("a second claim on one name succeeded")
	}
	if _, err := names.claimArm("PlanUpdate", "SessionUpdate", "another arm"); err == nil {
		t.Error("two arms of one union claimed the same qualified name")
	}
}

// A doc comment is the specification's prose with two punctuation changes. Both
// exist so that the comment linters stay on for the file whose comments are most
// worth checking, and neither may alter the text.
func TestDocCommentsQuoteTheSchema(t *testing.T) {
	tests := []struct {
		name        string
		symbol      string
		description string
		want        []string
	}{
		{
			name:        "the symbol name is prefixed, not woven in",
			symbol:      "ContentBlock",
			description: "Content blocks represent displayable information.",
			want:        []string{"ContentBlock — Content blocks represent displayable information."},
		},
		{
			name:        "a missing full stop is added",
			symbol:      "PromptRequest",
			description: "See protocol docs: [User Message](https://example.test/a)",
			want:        []string{"PromptRequest — See protocol docs: [User Message](https://example.test/a)."},
		},
		{
			name:        "an existing one is not doubled",
			symbol:      "T",
			description: "A sentence.",
			want:        []string{"T — A sentence."},
		},
		{
			name:        "the unstable marker leads, where upstream put it",
			symbol:      "LlmProtocol",
			description: "**UNSTABLE**\n\nNot part of the spec yet.",
			want:        []string{"LlmProtocol — **UNSTABLE**", "", "Not part of the spec yet."},
		},
		{
			name:        "line breaks are upstream's",
			symbol:      "",
			description: "Content blocks appear in:\n- prompts\n- updates",
			want:        []string{"Content blocks appear in:", "- prompts", "- updates."},
		},
		{
			name:        "trailing blank lines go",
			symbol:      "T",
			description: "One.\n\n\n",
			want:        []string{"T — One."},
		},
		{
			name:        "a definition with no description still says what it is",
			symbol:      "ExtRequest",
			description: "",
			want:        []string{"ExtRequest is a protocol type the schema defines without a description."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := docComment(test.symbol, test.description)
			if len(got) != len(test.want) {
				t.Fatalf("docComment produced %d lines %q, want %d %q",
					len(got), got, len(test.want), test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}

// A union's first line is generated rather than quoted, because it has to name
// the arms and the schema's description does not.
func TestUnionDocsNameTheArms(t *testing.T) {
	tests := []struct {
		arms []string
		want string
	}{
		{arms: []string{"TextContent"}, want: "A ContentBlock is one of [*TextContent]."},
		{arms: []string{"A", "B"}, want: "A ContentBlock is one of [*A] or [*B]."},
		{arms: []string{"A", "B", "C"}, want: "A ContentBlock is one of [*A], [*B] or [*C]."},
	}

	for _, test := range tests {
		got := unionDoc("ContentBlock", test.arms, "")
		if len(got) != 1 || got[0] != test.want {
			t.Errorf("unionDoc(%v) = %q, want [%q]", test.arms, got, test.want)
		}
	}
}

// Generated prose is wrapped; quoted prose is not. Only the first is this
// generator's to reflow.
func TestWrapBreaksOnWords(t *testing.T) {
	lines := wrap(20, "the schema's not clause reserves the known discriminant values")
	for _, line := range lines {
		if len(line) > 20 {
			t.Errorf("line %q is %d characters, over the width", line, len(line))
		}
	}
	if len(lines) < 3 {
		t.Errorf("wrapped into %d lines, expected the text to break", len(lines))
	}
}
