package web

import "testing"

func TestImageQuotaRefusal(t *testing.T) {
	for _, text := range []string{
		"Sorry, I can't generate any more images today.",
		"Sorry, try again tomorrow.",
		"抱歉，我今天无法再生成图片。请明天再试。",
	} {
		if !isImageQuotaRefusal(text) {
			t.Fatalf("quota refusal not detected: %q", text)
		}
	}
	if isImageQuotaRefusal("Here is your generated image.") {
		t.Fatal("ordinary image response misclassified")
	}
}

func TestClassifyImageFailure(t *testing.T) {
	tests := []struct {
		name string
		text string
		want imageFailureKind
	}{
		{name: "quota", text: "Sorry, I can't generate any more images today.", want: imageFailureQuota},
		{name: "high demand", text: "The image generation service is currently experiencing unusually high demand.", want: imageFailureBusy},
		{name: "temporary unavailable", text: "Image generation is temporarily unavailable. Please try again later.", want: imageFailureBusy},
		{name: "retry shortly", text: "Sorry, the image generation request could not be completed at this time. Please try again shortly.", want: imageFailureBusy},
		{name: "chinese busy", text: "图片生成服务当前繁忙，请稍后再试。", want: imageFailureBusy},
		{name: "ordinary response", text: "Here is your generated image.", want: imageFailureNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyImageFailure(tt.text); got != tt.want {
				t.Fatalf("classifyImageFailure(%q)=%d want %d", tt.text, got, tt.want)
			}
		})
	}
}
