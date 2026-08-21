package chathub

import (
	"errors"
	"testing"
)

func TestCompletionFrameFailurePreservesPartialSnapshot(t *testing.T) {
	cause := errors.New("completion failed")
	result, err := completionFrameFailure(Result{Text: "partial answer", RequestID: "request-1"}, cause)
	if !IsStreamInterrupted(err) || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	if !result.Incomplete || result.FailureStage != "completion_error" || result.Text != "partial answer" || result.RequestID != "request-1" {
		t.Fatalf("partial result = %#v", result)
	}
}

func TestCompletionFrameFailureWithoutPartialReturnsPlainFailure(t *testing.T) {
	cause := errors.New("completion failed")
	result, err := completionFrameFailure(Result{}, cause)
	if result.Text != "" || result.Reasoning != "" || result.Incomplete || result.FailureStage != "" || !errors.Is(err, cause) || IsStreamInterrupted(err) {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}
