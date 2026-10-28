package engine

import "testing"

func TestMatchAttachmentStrictVideo(t *testing.T) {
	video := Attachment{Src: "poster.png", VSrc: "clip.mp4", TID: "video", HasVideo: true}
	if !MatchAttachment(video, "video") {
		t.Fatalf("real video should match")
	}
	coverWithVideoTID := Attachment{Src: "cover.jpg", TID: "hatch-chat-attachment-presentation-video"}
	if MatchAttachment(coverWithVideoTID, "video") {
		t.Fatalf("a static cover must never satisfy a video request")
	}
	posterOnly := Attachment{Src: "poster.png", TID: "image"}
	if MatchAttachment(posterOnly, "video") {
		t.Fatalf("an image attachment must not satisfy video")
	}
	image := Attachment{Src: "result.webp", TID: "hatch-chat-attachment-presentation-image", HasImg: true}
	if !MatchAttachment(image, "image") {
		t.Fatalf("image attachment should match")
	}
	if MatchAttachment(video, "image") {
		t.Fatalf("video attachment must not match an image request")
	}
}

func TestMediaRefusalText(t *testing.T) {
	videoRefusal := "Video not generated successfully: this country or region is not available for video generation"
	if got := MediaRefusalText(videoRefusal, "video"); got == "" {
		t.Fatalf("expected a video refusal")
	}
	if got := MediaRefusalText("Settings\nRegion: US\nVideo", "video"); got != "" {
		t.Fatalf("generic page chrome must not count as refusal, got %q", got)
	}
	if got := MediaRefusalText("Could not generate image for this request", "image"); got == "" {
		t.Fatalf("expected an image refusal")
	}
	if got := MediaRefusalText("Everything is fine", "image"); got != "" {
		t.Fatalf("unexpected refusal: %q", got)
	}
}

func TestPickExt(t *testing.T) {
	cases := []struct{ mime, url, expect, want string }{
		{"video/mp4", "", "video", ".mp4"},
		{"image/webp", "", "image", ".webp"},
		{"image/jpeg", "", "image", ".jpg"},
		{"", "https://x/y.webm", "video", ".webm"},
		{"", "", "video", ".mp4"},
		{"", "", "image", ".png"},
	}
	for _, tc := range cases {
		if got := PickExt(tc.mime, tc.url, tc.expect); got != tc.want {
			t.Fatalf("PickExt(%q,%q,%q)=%q, want %q", tc.mime, tc.url, tc.expect, got, tc.want)
		}
	}
}

func TestIsGlobalIP(t *testing.T) {
	public := []string{"8.8.8.8", "2606:4700:4700::1111"}
	private := []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.1.1", "100.64.0.1", "::1", "fe80::1", "::ffff:127.0.0.1"}
	for _, s := range public {
		if !isGlobalIP(parseIPForTest(s)) {
			t.Fatalf("expected %s to be global", s)
		}
	}
	for _, s := range private {
		if isGlobalIP(parseIPForTest(s)) {
			t.Fatalf("expected %s to be denied", s)
		}
	}
}
