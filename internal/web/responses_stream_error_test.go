package web

import (
	"encoding/json"
	"testing"
)

func TestResponsesAdapterRecognizesOpenAIStreamErrorShape(t *testing.T) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(`{"error":{"message":"upstream disconnected","code":"stream_interrupted","partial":true}}`), &chunk); err != nil {
		t.Fatal(err)
	}
	failure, ok := chunk["error"].(map[string]any)
	if !ok {
		t.Fatalf("error frame not recognized: %#v", chunk)
	}
	if failure["code"] != "stream_interrupted" || failure["message"] != "upstream disconnected" {
		t.Fatalf("unexpected failure: %#v", failure)
	}
}
