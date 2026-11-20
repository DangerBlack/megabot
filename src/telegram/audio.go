package telegram

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/telebot.v3"
)

// transcribeExts are the formats the OpenAI transcription endpoint accepts.
// Anything else (e.g. raw .aac, .amr, .opus, .wma) is converted with ffmpeg.
var transcribeExts = map[string]bool{
	".flac": true, ".m4a": true, ".mp3": true, ".mp4": true, ".mpeg": true,
	".mpga": true, ".oga": true, ".ogg": true, ".wav": true, ".webm": true,
}

// audioMIMEExts maps common audio MIME types to an extension. Go's built-in
// table has no audio types and Alpine ships no /etc/mime.types.
var audioMIMEExts = map[string]string{
	"audio/aac": ".aac", "audio/x-aac": ".aac", "audio/aacp": ".aac",
	"audio/mp4": ".m4a", "audio/m4a": ".m4a", "audio/x-m4a": ".m4a",
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3",
	"audio/ogg": ".ogg", "audio/opus": ".opus",
	"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/wave": ".wav",
	"audio/flac": ".flac", "audio/x-flac": ".flac",
	"audio/webm": ".webm", "audio/amr": ".amr", "audio/x-ms-wma": ".wma",
	"video/mp4": ".mp4",
}

// audioOf extracts the audio attachment of a message: a voice note, an audio
// file, or a document whose MIME type or name says it is audio (Telegram
// often delivers .aac and similar files as documents).
func audioOf(m *telebot.Message) (f telebot.File, seconds int, filename string, ok bool) {
	switch {
	case m.Voice != nil:
		return m.Voice.File, m.Voice.Duration, "voice.ogg", true
	case m.Audio != nil:
		return m.Audio.File, m.Audio.Duration, audioFilename(m.Audio.FileName, m.Audio.MIME), true
	case m.Document != nil && isAudioDocument(m.Document):
		return m.Document.File, 0, audioFilename(m.Document.FileName, m.Document.MIME), true
	}
	return telebot.File{}, 0, "", false
}

func isAudioDocument(d *telebot.Document) bool {
	if strings.HasPrefix(d.MIME, "audio/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(d.FileName))
	if ext == ".mp4" || ext == ".webm" || ext == ".mpeg" { // usually video
		return false
	}
	if transcribeExts[ext] {
		return true
	}
	for _, e := range audioMIMEExts {
		if e == ext {
			return true
		}
	}
	return false
}

// audioFilename gives the API a name with an extension it recognises.
func audioFilename(name, mimeType string) string {
	if name != "" && filepath.Ext(name) != "" {
		return name
	}
	mt, _, _ := mime.ParseMediaType(mimeType)
	if ext, ok := audioMIMEExts[mt]; ok {
		return "audio" + ext
	}
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return "audio" + exts[0]
	}
	return "audio.mp3"
}

// prepareAudio returns the file unchanged when its format is accepted by the
// transcription API, otherwise converts it to mono 64 kbit/s MP3 with ffmpeg.
// seconds is estimated from the output size (0 if no conversion happened).
func prepareAudio(ctx context.Context, filename string, data []byte) (name string, out []byte, seconds int, err error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if transcribeExts[ext] {
		return filename, data, 0, nil
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", nil, 0, fmt.Errorf("formato %s non supportato e ffmpeg non è installato", ext)
	}
	// The input is streamed on stdin and only the pipe protocol is allowed,
	// so a crafted file (e.g. a playlist-like container) cannot make ffmpeg
	// open local files or network URLs. Formats that need seeking (m4a/mp4)
	// are accepted by the API as-is and never reach this point.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner",
		"-protocol_whitelist", "pipe", "-i", "pipe:0",
		"-vn", "-ac", "1", "-c:a", "libmp3lame", "-b:a", "64k", "-f", "mp3", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return "", nil, 0, fmt.Errorf("conversione audio fallita: %v: %s", err, msg)
	}
	// Output is constant 64 kbit/s, so its size gives the duration even when
	// the piped input carries none.
	seconds = int(float64(stdout.Len())*8/64000 + 0.5)
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	return base + ".mp3", stdout.Bytes(), seconds, nil
}
