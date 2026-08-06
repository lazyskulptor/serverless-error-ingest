package main

import (
	"strings"
	"testing"
)

func TestComputeIssueIDGroupsSimilarEvents(t *testing.T) {
	a := sentryEvent{
		Platform: "javascript",
		Exception: struct {
			Values []exceptionValue `json:"values"`
		}{
			Values: []exceptionValue{{
				Type:  "TypeError",
				Value: "Cannot read properties of undefined",
				Stacktrace: struct {
					Frames []stackFrame `json:"frames"`
				}{
					Frames: []stackFrame{{File: "app.js", Function: "render", Lineno: 42}},
				},
			}},
		},
	}
	b := a // identical crash
	b.Exception.Values[0].Value = "Cannot read properties of undefined (reading 'x')"

	idA := computeIssueID(a)
	idB := computeIssueID(b)
	if idA == "" || idB == "" {
		t.Fatal("empty issue ids")
	}
	if idA != idB {
		t.Errorf("similar events should group together: %s != %s", idA, idB)
	}

	// A different crash must produce a different issue id.
	c := a
	c.Exception.Values[0].Value = "a completely different error"
	if idC := computeIssueID(c); idC == idA {
		t.Error("different events must not group together")
	}
}

func TestComputeIssueIDNoException(t *testing.T) {
	ev := sentryEvent{Platform: "node", Message: "Something failed"}
	if id := computeIssueID(ev); id == "" {
		t.Fatal("empty issue id for message-only event")
	}
}

func TestSummarizeException(t *testing.T) {
	ev := sentryEvent{
		Exception: struct {
			Values []exceptionValue `json:"values"`
		}{
			Values: []exceptionValue{{Type: "TypeError", Value: "Cannot read properties of undefined"}},
		},
	}
	got := summarize(ev)
	if got != "TypeError: Cannot read properties of undefined" {
		t.Errorf("summarize = %q", got)
	}
}

func TestSummarizeMessageOnly(t *testing.T) {
	ev := sentryEvent{Message: "Something failed"}
	if got := summarize(ev); got != "Something failed" {
		t.Errorf("summarize = %q", got)
	}
}

func TestAIPromptNeverContainsRawPayload(t *testing.T) {
	ev := sentryEvent{
		Platform: "javascript",
		Message:  "SECRET_PAYLOAD_MARKER",
		Exception: struct {
			Values []exceptionValue `json:"values"`
		}{
			Values: []exceptionValue{{
				Type:  "TypeError",
				Value: "Cannot read properties",
				Stacktrace: struct {
					Frames []stackFrame `json:"frames"`
				}{
					Frames: []stackFrame{{File: "app.js", Function: "render", Lineno: 42}},
				},
			}},
		},
	}
	prompt := aiPrompt(ev)
	if strings.Contains(prompt, "SECRET_PAYLOAD_MARKER") {
		t.Error("AI prompt leaked the raw event message field")
	}
	if !strings.Contains(prompt, "TypeError") {
		t.Errorf("prompt should contain extracted type, got %q", prompt)
	}
}
