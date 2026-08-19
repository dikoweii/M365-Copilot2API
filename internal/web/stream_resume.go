package web

import (
	"fmt"
	"strings"
)

const (
	streamResumePromptRunes = 24000
	streamResumeTailRunes   = 6000
	streamOverlapRunes      = 3000
)

func streamResumePrompt(originalPrompt, partialText string) string {
	return fmt.Sprintf(`The previous response was interrupted after some text was already delivered to the client.

Continue the SAME answer from the exact point after PARTIAL ANSWER TAIL.
- Output continuation text only. No preface, apology, heading, recap, or explanation.
- Do not repeat text that appears in PARTIAL ANSWER TAIL.
- Preserve the language, format, names, facts, and style of the interrupted answer.
- Finish the original request completely.

ORIGINAL REQUEST AND CONTEXT:
%s

PARTIAL ANSWER TAIL:
%s`, compactResumeText(originalPrompt, streamResumePromptRunes), tailRunes(partialText, streamResumeTailRunes))
}

func compactResumeText(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return string(runes)
	}
	half := maxRunes / 2
	return string(runes[:half]) + "\n...[context omitted for bounded resume]...\n" + string(runes[len(runes)-(maxRunes-half):])
}

func tailRunes(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[len(runes)-maxRunes:])
}

// trimStreamResumeOverlap removes text repeated at the boundary between a
// delivered partial answer and a continuation. It compares runes so CJK text
// cannot be split in the middle of a UTF-8 sequence.
func trimStreamResumeOverlap(partialText, continuation string) (string, int) {
	partial := []rune(partialText)
	next := []rune(strings.TrimLeft(continuation, "\r\n"))
	limit := len(partial)
	if len(next) < limit {
		limit = len(next)
	}
	if limit > streamOverlapRunes {
		limit = streamOverlapRunes
	}
	for overlap := limit; overlap > 0; overlap-- {
		if string(partial[len(partial)-overlap:]) == string(next[:overlap]) {
			return string(next[overlap:]), overlap
		}
	}
	return string(next), 0
}
