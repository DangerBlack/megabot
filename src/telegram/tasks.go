package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"megabot/src/database"
	"megabot/src/openai"
	"mime"
	"strings"
	"unicode/utf8"

	"gopkg.in/telebot.v3"
)

const (
	summaryPrompt       = "Please write a short summary of this article, use the language of the article for the summary\n\n"
	ocrPrompt           = "Trascrivi fedelmente tutto il testo presente in questa immagine, mantenendo la lingua originale e l'impaginazione dove possibile. Restituisci solo il testo."
	transcriptSumPrompt = "Riassumi in modo conciso questa trascrizione di un messaggio vocale, nella sua stessa lingua. Metti in evidenza richieste, decisioni e cose da fare, se ce ne sono.\n\n"

	// transcriptions longer than this get a "Riassumi" button
	summarizeButtonRunes = 600
	// total characters of past conversation sent as context
	historyMaxChars = 100000
)

var errDisabled = errors.New("funzione non configurata su questo bot")

// spend describes what a task consumed, for usage logging and budgets.
type spend struct {
	Provider     string
	Model        string
	TokensIn     int
	TokensOut    int
	AudioSeconds int
	Images       int
	Cost         float64
}

func chatSpend(l *LLM, model string, r openai.Result) spend {
	return spend{Provider: l.Name, Model: model, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: l.Cost(r.TokensIn, r.TokensOut)}
}

// paidFor reports whether a task of this kind costs money for user u.
func (t *Telegram) paidFor(u *database.User, kind Kind) bool {
	switch kind {
	case KindTranscribe:
		return t.Cfg.TranscribeProvider != "" && t.Cfg.TranscribePricePerMin > 0
	case KindImage, KindImageRaw:
		return t.Cfg.ImageProvider != "" && t.Cfg.ImagePrice > 0
	}
	l := t.llmFor(u)
	return l != nil && l.Paid()
}

func (t *Telegram) checkBudget(c telebot.Context, u *database.User, kind Kind) error {
	if t.isRoot(c) || u == nil || u.BudgetUSD == nil || !t.paidFor(u, kind) {
		return nil
	}
	spent, err := t.DB.MonthSpent(u.ID)
	if err != nil {
		return err
	}
	return budgetError(spent, *u.BudgetUSD, t.hasFreeLLM())
}

// budgetError is nil while spent < budget.
func budgetError(spent, budget float64, freeAlternative bool) error {
	if spent < budget {
		return nil
	}
	msg := fmt.Sprintf("budget mensile esaurito ($%.2f su $%.2f)", spent, budget)
	if freeAlternative {
		msg += ". Con /llm puoi passare a un modello gratuito per testo e foto"
	}
	return errors.New(msg)
}

func (t *Telegram) hasFreeLLM() bool {
	for _, l := range t.LLMs {
		if !l.Paid() {
			return true
		}
	}
	return false
}

func (t *Telegram) download(f telebot.File) ([]byte, error) {
	rc, err := t.Bot.File(&f)
	if err != nil {
		return nil, fmt.Errorf("download da Telegram fallito: %w", err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// historyMessages turns a stored thread into chat messages, merging
// consecutive parts of the same role and dropping the oldest messages when
// the total exceeds maxChars.
func historyMessages(thread []database.Message, maxChars int) []openai.Message {
	type turn struct{ role, content string }
	var turns []turn
	for _, m := range thread {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].content += "\n" + m.Content
			continue
		}
		turns = append(turns, turn{role, m.Content})
	}
	total := 0
	start := len(turns)
	for start > 0 {
		n := utf8.RuneCountInString(turns[start-1].content)
		if total+n > maxChars && start < len(turns) {
			break
		}
		total += n
		start--
	}
	out := make([]openai.Message, 0, len(turns)-start)
	for _, tr := range turns[start:] {
		out = append(out, openai.Message{Role: tr.role, Content: tr.content})
	}
	return out
}

// history rebuilds the conversation the user is replying to.
func (t *Telegram) history(chatID int64, replyTo *telebot.Message) []openai.Message {
	if replyTo == nil {
		return nil
	}
	thread, err := t.DB.Thread(chatID, replyTo.ID, t.Cfg.HistoryMaxMessages)
	if err != nil || len(thread) == 0 {
		// not in the DB (older than retention, or someone else's message): use its text
		text := replyTo.Text
		if text == "" {
			text = replyTo.Caption
		}
		if text == "" {
			return nil
		}
		role := "user"
		if replyTo.Sender != nil && t.Bot.Me != nil && replyTo.Sender.ID == t.Bot.Me.ID {
			role = "assistant"
		}
		thread = []database.Message{{Role: role, Content: text}}
	}
	return historyMessages(thread, historyMaxChars)
}

func replyToID(m *telebot.Message) int {
	if m.ReplyTo != nil {
		return m.ReplyTo.ID
	}
	return 0
}

func (t *Telegram) chat(ctx context.Context, c telebot.Context, u *database.User, prompt string) (spend, error) {
	l := t.llmFor(u)
	m := c.Message()
	msgs := append(t.history(c.Chat().ID, m.ReplyTo), openai.Message{Role: "user", Content: prompt})
	res, err := l.Client.Chat(ctx, l.ChatModel, msgs)
	if err != nil {
		return spend{}, err
	}
	sent, err := t.replyLong(c, nil, res.Text, nil)
	t.remember(c.Chat().ID, m.ID, replyToID(m), prompt, sent)
	return chatSpend(l, l.ChatModel, res), err
}

func (t *Telegram) image(ctx context.Context, c telebot.Context, prompt string, raw bool) (spend, error) {
	l := t.LLMs[t.Cfg.ImageProvider]
	if l == nil {
		return spend{}, errDisabled
	}
	png, err := l.Client.GenerateImage(ctx, t.Cfg.ImageModel, prompt)
	if err != nil {
		return spend{}, err
	}
	file := telebot.FromReader(bytes.NewReader(png))
	var what telebot.Sendable = &telebot.Photo{File: file}
	if raw {
		what = &telebot.Document{File: file, FileName: "image.png", MIME: "image/png"}
	}
	_, err = t.Bot.Reply(c.Message(), what)
	return spend{Provider: l.Name, Model: t.Cfg.ImageModel, Images: 1, Cost: t.Cfg.ImagePrice}, err
}

func (t *Telegram) transcribe(ctx context.Context, c telebot.Context, m *telebot.Message) (spend, error) {
	l := t.LLMs[t.Cfg.TranscribeProvider]
	if l == nil {
		return spend{}, errDisabled
	}
	var f telebot.File
	var seconds int
	filename := "voice.ogg"
	if m.Voice != nil {
		f, seconds = m.Voice.File, m.Voice.Duration
	} else {
		f, seconds = m.Audio.File, m.Audio.Duration
		filename = audioFilename(m.Audio.FileName, m.Audio.MIME)
	}
	data, err := t.download(f)
	if err != nil {
		return spend{}, err
	}
	text, err := l.Client.Transcribe(ctx, t.Cfg.TranscribeModel, filename, bytes.NewReader(data))
	if err != nil {
		return spend{}, err
	}
	sp := spend{Provider: l.Name, Model: t.Cfg.TranscribeModel, AudioSeconds: seconds,
		Cost: float64(seconds) / 60 * t.Cfg.TranscribePricePerMin}
	if text == "" {
		_, err = t.replyLong(c, nil, "(nessun parlato riconosciuto)", nil)
		return sp, err
	}
	var markup *telebot.ReplyMarkup
	if utf8.RuneCountInString(text) > summarizeButtonRunes {
		markup = &telebot.ReplyMarkup{InlineKeyboard: [][]telebot.InlineButton{{{Text: "📝 Riassumi", Data: "sum"}}}}
	}
	sent, err := t.replyLong(c, nil, text, markup)
	t.remember(c.Chat().ID, m.ID, replyToID(m), "[messaggio vocale]", sent)
	return sp, err
}

// audioFilename gives the API a name with an extension it recognises.
func audioFilename(name, mimeType string) string {
	if name != "" && strings.Contains(name, ".") {
		return name
	}
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return "audio" + exts[0]
	}
	return "audio.mp3"
}

func (t *Telegram) summary(ctx context.Context, c telebot.Context, u *database.User, url string) (spend, error) {
	page, err := t.Fetcher.Fetch(ctx, url)
	if err != nil {
		return spend{}, fmt.Errorf("non riesco a leggere la pagina: %w", err)
	}
	l := t.llmFor(u)
	res, err := l.Client.Ask(ctx, l.SummaryModel, summaryPrompt+page.Text)
	if err != nil {
		return spend{}, err
	}
	text := res.Text
	if page.Title != "" {
		text = "📰 " + page.Title + "\n\n" + text
	}
	if page.Via != "direct" {
		text += "\n\n(letta tramite " + page.Via + ")"
	}
	m := c.Message()
	sent, err := t.replyLong(c, nil, text, nil)
	// the full article goes in the thread so replies can ask about details
	t.remember(c.Chat().ID, m.ID, replyToID(m), "Articolo da "+url+":\n\n"+page.Text+"\n\nRiassumilo.", sent)
	return chatSpend(l, l.SummaryModel, res), err
}

func (t *Telegram) photo(ctx context.Context, c telebot.Context, u *database.User, m *telebot.Message, prompt, remembered string) (spend, error) {
	// telebot exposes the largest available size as Message.Photo
	img, err := t.download(m.Photo.File)
	if err != nil {
		return spend{}, err
	}
	l := t.llmFor(u)
	res, err := l.Client.Vision(ctx, l.VisionModel, prompt, img, "image/jpeg")
	if err != nil {
		return spend{}, err
	}
	sent, err := t.replyLong(c, nil, res.Text, nil)
	t.remember(c.Chat().ID, m.ID, replyToID(m), remembered, sent)
	return chatSpend(l, l.VisionModel, res), err
}

// summarizeTranscript handles the "Riassumi" button under a transcription.
func (t *Telegram) summarizeTranscript(ctx context.Context, c telebot.Context, u *database.User) (spend, error) {
	cb := c.Callback()
	chatID := cb.Message.Chat.ID
	transcript := cb.Message.Text
	if thread, err := t.DB.Thread(chatID, cb.Message.ID, 50); err == nil && len(thread) > 0 {
		// the transcription is the trailing run of assistant messages
		var parts []string
		for i := len(thread) - 1; i >= 0 && thread[i].Role == "assistant"; i-- {
			parts = append([]string{thread[i].Content}, parts...)
		}
		if len(parts) > 0 {
			transcript = strings.Join(parts, "\n")
		}
	}
	l := t.llmFor(u)
	res, err := l.Client.Ask(ctx, l.SummaryModel, transcriptSumPrompt+transcript)
	if err != nil {
		return spend{}, err
	}
	_, _ = t.Bot.EditReplyMarkup(cb.Message, nil)
	sent, err := t.replyLong(c, cb.Message, "📝 "+res.Text, nil)
	// chained to the transcription, so a reply to the summary still sees the full transcript
	t.rememberAnswer(chatID, cb.Message.ID, sent)
	return chatSpend(l, l.SummaryModel, res), err
}
