package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Request
	}{
		{"photo ocr legacy caption", Input{Text: "transcribe", HasPhoto: true}, Request{Kind: KindOCR}},
		{"photo ocr italian", Input{Text: " Trascrivi ", HasPhoto: true}, Request{Kind: KindOCR}},
		{"photo without caption is ocr", Input{HasPhoto: true}, Request{Kind: KindOCR}},
		{"photo question", Input{Text: "quanto costa in totale?", HasPhoto: true}, Request{Kind: KindPhoto, Text: "quanto costa in totale?"}},
		{"photo wins over command caption", Input{Text: "/image gatto", HasPhoto: true}, Request{Kind: KindPhoto, Text: "/image gatto"}},
		{"voice", Input{HasAudio: true}, Request{Kind: KindTranscribe}},
		{"gpt", Input{Text: "/gpt come stai?"}, Request{Kind: KindChat, Text: "come stai?"}},
		{"gpt with bot name", Input{Text: "/gpt@megabot ciao"}, Request{Kind: KindChat, Text: "ciao"}},
		{"gpt multiline", Input{Text: "/gpt riga1\nriga2"}, Request{Kind: KindChat, Text: "riga1\nriga2"}},
		{"gpt without prompt", Input{Text: "/gpt"}, Request{}},
		{"image", Input{Text: "/image un gatto"}, Request{Kind: KindImage, Text: "un gatto"}},
		{"image raw", Input{Text: "/image-raw un gatto"}, Request{Kind: KindImageRaw, Text: "un gatto"}},
		{"image raw underscore", Input{Text: "/image_raw un gatto"}, Request{Kind: KindImageRaw, Text: "un gatto"}},
		{"unknown command", Input{Text: "/foo bar", Private: true}, Request{}},
		{"command with link is not a summary", Input{Text: "/gpt riassumi https://x.it"}, Request{Kind: KindChat, Text: "riassumi https://x.it"}},
		{"link", Input{Text: "leggi qui https://example.com/art"}, Request{Kind: KindSummary, URL: "https://example.com/art"}},
		{"link in group", Input{Text: "https://example.com", Private: false}, Request{Kind: KindSummary, URL: "https://example.com"}},
		{"plain text private", Input{Text: "ciao", Private: true}, Request{Kind: KindChat, Text: "ciao"}},
		{"plain text group ignored", Input{Text: "ciao"}, Request{}},
		{"reply to bot in group", Input{Text: "e poi?", ReplyToBot: true}, Request{Kind: KindChat, Text: "e poi?"}},
		{"empty", Input{Private: true}, Request{}},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }

func TestSplitMessageShort(t *testing.T) {
	if got := SplitMessage("ciao", 10); len(got) != 1 || got[0] != "ciao" {
		t.Fatalf("got %q", got)
	}
	if got := SplitMessage("", 10); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestSplitMessagePrefersNewlines(t *testing.T) {
	s := strings.Repeat("a", 8) + "\n" + strings.Repeat("b", 8)
	got := SplitMessage(s, 10)
	if len(got) != 2 || got[0] != strings.Repeat("a", 8) || got[1] != strings.Repeat("b", 8) {
		t.Fatalf("got %q", got)
	}
}

func TestSplitMessageHardCutKeepsEverything(t *testing.T) {
	s := strings.Repeat("x", 25)
	got := SplitMessage(s, 10)
	if strings.Join(got, "") != s || len(got) != 3 {
		t.Fatalf("got %q", got)
	}
}

func TestSplitMessageCountsUTF16(t *testing.T) {
	s := strings.Repeat("😀", 7) // 14 UTF-16 units
	got := SplitMessage(s, 10)
	for _, p := range got {
		if units(p) > 10 {
			t.Fatalf("chunk %q has %d units", p, units(p))
		}
	}
	if strings.Join(got, "") != s {
		t.Fatalf("lost characters: %q", got)
	}
}
