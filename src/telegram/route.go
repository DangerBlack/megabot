package telegram

import (
	"megabot/src/webpage"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Kind is what a message asks for. The string values are what gets logged in
// the usage table (same names the n8n workflow used, plus "image-raw" and "ocr").
type Kind string

const (
	KindNone       Kind = ""
	KindChat       Kind = "text"
	KindImage      Kind = "image"
	KindImageRaw   Kind = "image-raw"
	KindTranscribe Kind = "audio"
	KindSummary    Kind = "url"
	KindOCR        Kind = "ocr"
	KindPhoto      Kind = "photo"         // question about a photo (caption)
	KindAudioSum   Kind = "audio-summary" // "Riassumi" button under a transcription
)

type Input struct {
	Text       string // message text, or the caption for media
	HasPhoto   bool
	HasAudio   bool // voice note or audio file
	Private    bool // private chat with the bot
	ReplyToBot bool // the message replies to one of the bot's messages
}

type Request struct {
	Kind Kind
	Text string // prompt / question
	URL  string // for KindSummary
}

// /cmd, /cmd@botname, /image-raw (not a valid Telegram command, so we parse it ourselves)
var cmdRx = regexp.MustCompile(`^/([A-Za-z0-9_-]+)(?:@\w+)?(?:\s+([\s\S]*))?$`)

// Classify decides what to do with a message. Precedence: photo > audio >
// /gpt, /image, /image-raw > link > plain text. Plain text is answered in
// private chats, or in groups when it replies to the bot, so the bot stays
// quiet in group conversations.
func Classify(in Input) Request {
	text := strings.TrimSpace(in.Text)
	if in.HasPhoto {
		switch strings.ToLower(text) {
		case "", "transcribe", "trascrivi":
			return Request{Kind: KindOCR}
		}
		return Request{Kind: KindPhoto, Text: text}
	}
	if in.HasAudio {
		return Request{Kind: KindTranscribe}
	}
	if m := cmdRx.FindStringSubmatch(text); m != nil {
		arg := strings.TrimSpace(m[2])
		if arg == "" {
			return Request{}
		}
		switch strings.ToLower(m[1]) {
		case "gpt":
			return Request{Kind: KindChat, Text: arg}
		case "image":
			return Request{Kind: KindImage, Text: arg}
		case "image-raw", "image_raw":
			return Request{Kind: KindImageRaw, Text: arg}
		}
		return Request{}
	}
	if u := webpage.FirstURL(text); u != "" {
		return Request{Kind: KindSummary, URL: u}
	}
	if (in.Private || in.ReplyToBot) && text != "" {
		return Request{Kind: KindChat, Text: text}
	}
	return Request{}
}

// TelegramLimit is the max message length, counted in UTF-16 code units.
const TelegramLimit = 4096

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// SplitMessage splits s into chunks of at most limit UTF-16 units, preferring
// newline boundaries. No characters are lost except the newline used as a cut.
func SplitMessage(s string, limit int) []string {
	var parts []string
	for s != "" {
		units, cut := 0, len(s)
		for i, r := range s {
			if units+utf16Len(r) > limit {
				cut = i
				break
			}
			units += utf16Len(r)
		}
		if cut == len(s) {
			parts = append(parts, s)
			break
		}
		if nl := strings.LastIndexByte(s[:cut], '\n'); nl > cut/2 {
			parts = append(parts, s[:nl])
			s = s[nl+1:]
			continue
		}
		parts = append(parts, s[:cut])
		s = s[cut:]
	}
	return parts
}

// shorten trims s to n runes for error messages.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
