package web

import (
	"strings"
	"testing"
)

func TestTrimStreamResumeOverlapHandlesCJKBoundary(t *testing.T) {
	partial := "黑雨落下，陆沉抬起头。"
	continuation := "陆沉抬起头。远处的山门正在打开。"
	got, overlap := trimStreamResumeOverlap(partial, continuation)
	if got != "远处的山门正在打开。" {
		t.Fatalf("continuation = %q", got)
	}
	if overlap != len([]rune("陆沉抬起头。")) {
		t.Fatalf("overlap = %d", overlap)
	}
}

func TestTrimStreamResumeOverlapPreservesNonOverlappingText(t *testing.T) {
	got, overlap := trimStreamResumeOverlap("上一段已经结束。", "\n下一段从这里开始。")
	if got != "下一段从这里开始。" || overlap != 0 {
		t.Fatalf("got %q overlap=%d", got, overlap)
	}
}

func TestStreamResumePromptIsBoundedAndContinuationOnly(t *testing.T) {
	original := strings.Repeat("上下文", streamResumePromptRunes)
	partial := strings.Repeat("正文", streamResumeTailRunes)
	prompt := streamResumePrompt(original, partial)
	if !strings.Contains(prompt, "Output continuation text only") || !strings.Contains(prompt, "PARTIAL ANSWER TAIL") {
		t.Fatalf("resume instructions missing: %s", prompt[:200])
	}
	if len([]rune(prompt)) > streamResumePromptRunes+streamResumeTailRunes+1000 {
		t.Fatalf("resume prompt is not bounded: %d runes", len([]rune(prompt)))
	}
}
