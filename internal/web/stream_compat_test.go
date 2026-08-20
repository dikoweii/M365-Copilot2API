package web

import (
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestCompatibilityStreamFiltersAlreadyStreamedText(t *testing.T) {
	if emitCompatibilityEvent(chathub.Event{Kind: "update"}) {
		t.Fatal("raw update frames can duplicate text already sent as delta")
	}
	if !emitCompatibilityEvent(chathub.Event{Kind: "complete"}) {
		t.Fatal("completion metadata should remain available")
	}
	if emitCompatibilitySemantic(chathub.SemanticEvent{Kind: "message"}) {
		t.Fatal("plain semantic messages can duplicate streamed assistant text")
	}
	if !emitCompatibilitySemantic(chathub.SemanticEvent{Kind: "tool.progress", MessageType: "Progress"}) {
		t.Fatal("tool progress must remain available to compatibility clients")
	}
}
