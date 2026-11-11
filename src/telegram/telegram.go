package telegram

import (
	"context"
	"errors"
	"fmt"
	"log"
	"megabot/src/config"
	"megabot/src/database"
	"megabot/src/openai"
	"megabot/src/webpage"
	"net/http"
	"strings"
	"time"

	"gopkg.in/telebot.v3"
)

const requestTimeout = 5 * time.Minute

// LLM is a configured provider with its client.
type LLM struct {
	config.Provider
	Client *openai.Client
}

type Telegram struct {
	Bot     *telebot.Bot
	DB      *database.Database
	Cfg     config.Config
	LLMs    map[string]*LLM
	Fetcher *webpage.Fetcher
}

func New(bot *telebot.Bot, db *database.Database, cfg config.Config) *Telegram {
	llms := map[string]*LLM{}
	for name, p := range cfg.Providers {
		llms[name] = &LLM{Provider: p, Client: openai.New(p.APIKey, p.BaseURL)}
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	fetcher := &webpage.Fetcher{Client: httpClient, MaxRunes: cfg.SummaryMaxChars}
	if cfg.FetchFallback == "jina" {
		fetcher.Fallback = &webpage.Jina{Client: &http.Client{Timeout: 60 * time.Second}, APIKey: cfg.JinaAPIKey}
	}
	return &Telegram{Bot: bot, DB: db, Cfg: cfg, LLMs: llms, Fetcher: fetcher}
}

func (t *Telegram) SetupHandlers() {
	t.Bot.Use(t.auth)
	t.Bot.Handle("/start", t.Help)
	t.Bot.Handle("/help", t.Help)
	t.Bot.Handle("/login", t.Login)
	t.Bot.Handle("/usage", t.Usage)
	t.Bot.Handle("/llm", t.SetLLM)
	t.Bot.Handle("/users", t.Users)
	t.Bot.Handle("/revoke", t.Revoke)
	t.Bot.Handle("/budget", t.Budget)
	t.Bot.Handle(telebot.OnCallback, t.OnCallback)
	t.Bot.Handle(telebot.OnText, t.OnMessage)
	t.Bot.Handle(telebot.OnPhoto, t.OnMessage)
	t.Bot.Handle(telebot.OnVoice, t.OnMessage)
	t.Bot.Handle(telebot.OnAudio, t.OnMessage)
}

const ctxUser = "user"

// userOf returns the *database.User the auth middleware attached (may be nil
// for /start, /help and /login from unknown people).
func userOf(c telebot.Context) *database.User {
	u, _ := c.Get(ctxUser).(*database.User)
	return u
}

func (t *Telegram) isRoot(c telebot.Context) bool {
	return c.Sender() != nil && c.Sender().ID == t.Cfg.RootUserID
}

// auth lets through root and approved users. Unknown people may only use
// /start, /help and /login; in private chats they get a hint, in groups nothing.
func (t *Telegram) auth(next telebot.HandlerFunc) telebot.HandlerFunc {
	return func(c telebot.Context) error {
		s := c.Sender()
		if s == nil || s.IsBot {
			return nil
		}
		if s.ID == t.Cfg.RootUserID {
			u, err := t.DB.GetUser(s.ID)
			if err == nil && (u == nil || u.Username != s.Username || u.Status != database.StatusApproved) {
				err = t.DB.EnsureApproved(s.ID, s.Username, fullName(s))
				if err == nil {
					u, err = t.DB.GetUser(s.ID)
				}
			}
			if err != nil {
				log.Printf("auth: root user: %v", err)
				return nil
			}
			c.Set(ctxUser, u)
			return next(c)
		}

		u, err := t.DB.GetUser(s.ID)
		if err != nil {
			log.Printf("auth: %v", err)
			return nil
		}
		if u != nil && u.Status == database.StatusApproved {
			c.Set(ctxUser, u)
			return next(c)
		}
		if c.Callback() != nil {
			return c.Respond(&telebot.CallbackResponse{Text: "Non sei autorizzato"})
		}
		if m := c.Message(); m != nil {
			switch commandOf(m.Text) {
			case "start", "help", "login":
				c.Set(ctxUser, u)
				return next(c)
			}
			if m.Private() && (u == nil || u.Status == database.StatusPending) {
				return c.Send(lockedText(u))
			}
		}
		log.Printf("ignored update from user id %d (@%s)", s.ID, s.Username)
		return nil
	}
}

func lockedText(u *database.User) string {
	if u != nil && u.Status == database.StatusPending {
		return "⏳ La tua richiesta di accesso è in attesa di approvazione."
	}
	return "🔒 Non sei ancora autorizzato. Scrivi /login per chiedere l'accesso."
}

func fullName(u *telebot.User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func displayName(u *telebot.User) string {
	name := fullName(u)
	if u.Username != "" {
		if name != "" {
			return fmt.Sprintf("%s (@%s)", name, u.Username)
		}
		return "@" + u.Username
	}
	if name != "" {
		return name
	}
	return fmt.Sprintf("user_%d", u.ID)
}

func userName(u *telebot.User) string {
	if u.Username != "" {
		return u.Username
	}
	if n := fullName(u); n != "" {
		return n
	}
	return fmt.Sprintf("user_%d", u.ID)
}

func (t *Telegram) Help(c telebot.Context) error {
	u := userOf(c)
	if u == nil || u.Status != database.StatusApproved {
		if u != nil && u.Status == database.StatusRejected {
			return nil
		}
		return c.Send("Ciao! Sono un assistente privato.\n\n" + lockedText(u))
	}
	llm := t.llmFor(u)
	var b strings.Builder
	b.WriteString(`Ecco cosa so fare:

🎙 Vocale o audio → lo trascrivo (con bottone "Riassumi" se è lungo)
🔗 Un link → riassumo la pagina; rispondi al riassunto per fare domande sull'articolo
📷 Foto senza didascalia → trascrivo il testo che contiene
📷 Foto con didascalia → la didascalia è la domanda sulla foto
🎨 /image <descrizione> → genero un'immagine (/image-raw per il file PNG)
💬 /gpt <domanda> → rispondo (in privato basta scrivermi)
↩️ Rispondi a un mio messaggio per continuare la conversazione
🧠 /llm → scegli il modello`)
	fmt.Fprintf(&b, " (ora: %s)\n📊 /usage → le tue statistiche e la spesa del mese", llm.Name)
	if t.isRoot(c) {
		b.WriteString(`

👑 Amministrazione
/users → utenti, stato, budget e spesa
/revoke <id> → revoca l'accesso
/budget <id> <usd|none> → imposta il budget mensile`)
	}
	return c.Send(b.String())
}

// llmFor returns the provider chosen by the user, or the default.
func (t *Telegram) llmFor(u *database.User) *LLM {
	if u != nil {
		if l, ok := t.LLMs[u.LLM]; ok {
			return l
		}
	}
	return t.LLMs[t.Cfg.DefaultLLM]
}

func (t *Telegram) OnMessage(c telebot.Context) error {
	m := c.Message()
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	req := Classify(Input{
		Text:       text,
		HasPhoto:   m.Photo != nil,
		HasAudio:   m.Voice != nil || m.Audio != nil,
		Private:    m.Private(),
		ReplyToBot: m.ReplyTo != nil && m.ReplyTo.Sender != nil && t.Bot.Me != nil && m.ReplyTo.Sender.ID == t.Bot.Me.ID,
	})
	if req.Kind == KindNone {
		return nil
	}
	return t.run(c, req.Kind, func(ctx context.Context, u *database.User) (spend, error) {
		switch req.Kind {
		case KindChat:
			return t.chat(ctx, c, u, req.Text)
		case KindImage, KindImageRaw:
			return t.image(ctx, c, req.Text, req.Kind == KindImageRaw)
		case KindTranscribe:
			return t.transcribe(ctx, c, m)
		case KindSummary:
			return t.summary(ctx, c, u, req.URL)
		case KindOCR:
			return t.photo(ctx, c, u, m, ocrPrompt, "[foto] trascrivi il testo")
		case KindPhoto:
			return t.photo(ctx, c, u, m, req.Text, "[foto] "+req.Text)
		}
		return spend{}, nil
	})
}

func (t *Telegram) OnCallback(c telebot.Context) error {
	data := c.Callback().Data
	switch {
	case data == "sum":
		_ = c.Respond()
		return t.run(c, KindAudioSum, func(ctx context.Context, u *database.User) (spend, error) {
			return t.summarizeTranscript(ctx, c, u)
		})
	case strings.HasPrefix(data, "adm:"):
		return t.adminDecision(c, data)
	}
	return c.Respond()
}

// run wraps a task with budget check, typing indicator, timeout, error
// reporting and usage logging.
func (t *Telegram) run(c telebot.Context, kind Kind, task func(context.Context, *database.User) (spend, error)) error {
	u := userOf(c)
	if err := t.checkBudget(c, u, kind); err != nil {
		return t.replyError(c, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	stop := t.keepChatAction(c.Chat(), actionFor(kind))
	start := time.Now()
	sp, err := task(ctx, u)
	stop()
	log.Printf("%s from %d via %s in %s cost=$%.4f err=%v", kind, c.Sender().ID, sp.Provider,
		time.Since(start).Round(time.Millisecond), sp.Cost, err)
	if err != nil {
		return t.replyError(c, err)
	}
	if err := t.DB.LogUsage(database.UsageEntry{
		UserID: c.Sender().ID, UserName: userName(c.Sender()), ChatID: c.Chat().ID, Kind: string(kind),
		Provider: sp.Provider, Model: sp.Model, TokensIn: sp.TokensIn, TokensOut: sp.TokensOut,
		AudioSeconds: sp.AudioSeconds, Images: sp.Images, CostUSD: sp.Cost,
	}); err != nil {
		log.Printf("usage log failed: %v", err)
	}
	return nil
}

func (t *Telegram) replyError(c telebot.Context, err error) error {
	msg := "⚠️ " + userError(err)
	if c.Callback() != nil {
		_, e := t.Bot.Reply(c.Callback().Message, msg)
		return e
	}
	return c.Reply(msg)
}

func actionFor(k Kind) telebot.ChatAction {
	switch k {
	case KindImage:
		return telebot.UploadingPhoto
	case KindImageRaw:
		return telebot.UploadingDocument
	}
	return telebot.Typing
}

// keepChatAction refreshes the "typing…" indicator (it expires after ~5s) until stop is called.
func (t *Telegram) keepChatAction(chat *telebot.Chat, action telebot.ChatAction) (stop func()) {
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(4 * time.Second)
		defer tick.Stop()
		for {
			_ = t.Bot.Notify(chat, action)
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() { close(done) }
}

func userError(err error) string {
	var oe *openai.Error
	switch {
	case errors.As(err, &oe):
		return fmt.Sprintf("Errore del modello (%d): %s", oe.Status, shorten(oe.Message, 300))
	case errors.Is(err, context.DeadlineExceeded):
		return "ci ho messo troppo, riprova"
	}
	return shorten(err.Error(), 300)
}

type sentPart struct {
	Msg  *telebot.Message
	Text string
}

// replyLong replies to the triggering message (or to `to` when given),
// splitting long text; markup goes on the last part.
func (t *Telegram) replyLong(c telebot.Context, to *telebot.Message, text string, markup *telebot.ReplyMarkup) ([]sentPart, error) {
	if strings.TrimSpace(text) == "" {
		text = "(risposta vuota)"
	}
	if to == nil {
		to = c.Message()
	}
	parts := SplitMessage(text, TelegramLimit)
	var sent []sentPart
	for i, part := range parts {
		opts := []any{telebot.NoPreview}
		if i == len(parts)-1 && markup != nil {
			opts = append(opts, markup)
		}
		var msg *telebot.Message
		var err error
		if i == 0 {
			msg, err = t.Bot.Reply(to, part, opts...)
		} else {
			msg, err = t.Bot.Send(c.Chat(), part, opts...)
		}
		if err != nil {
			return sent, err
		}
		sent = append(sent, sentPart{Msg: msg, Text: part})
	}
	return sent, nil
}

// remember stores the user's message and the bot's answer parts so that a
// later reply can rebuild the conversation.
func (t *Telegram) remember(chatID int64, userMsgID, userReplyTo int, userContent string, sent []sentPart) {
	if err := t.DB.SaveMessage(chatID, database.Message{MessageID: userMsgID, Role: "user", Content: userContent, ReplyTo: userReplyTo}); err != nil {
		log.Printf("save message: %v", err)
	}
	t.rememberAnswer(chatID, userMsgID, sent)
}

// rememberAnswer stores the bot's answer parts as a chain starting from parent.
func (t *Telegram) rememberAnswer(chatID int64, parent int, sent []sentPart) {
	prev := parent
	for _, p := range sent {
		if err := t.DB.SaveMessage(chatID, database.Message{MessageID: p.Msg.ID, Role: "assistant", Content: p.Text, ReplyTo: prev}); err != nil {
			log.Printf("save message: %v", err)
		}
		prev = p.Msg.ID
	}
}

// PruneLoop deletes old conversation messages once a day.
func (t *Telegram) PruneLoop(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		if n, err := t.DB.PruneMessages(t.Cfg.MessageRetentionDays); err != nil {
			log.Printf("prune: %v", err)
		} else if n > 0 {
			log.Printf("pruned %d old messages", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
