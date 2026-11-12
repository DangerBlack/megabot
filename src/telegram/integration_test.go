package telegram

// End-to-end tests: real handlers, a fake Telegram Bot API server and fake
// OpenAI-compatible servers.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"megabot/src/config"
	"megabot/src/database"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gopkg.in/telebot.v3"
)

const (
	rootID  = int64(1)
	bobID   = int64(7)
	botID   = int64(999)
	tgToken = "TEST:TOKEN"
)

type tgCall struct {
	Method string
	Params map[string]string
}

// fakeTelegram records every Bot API call and answers like Telegram would.
type fakeTelegram struct {
	mu     sync.Mutex
	calls  []tgCall
	nextID int
	srv    *httptest.Server
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{nextID: 1000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/file/") {
		w.Write([]byte("FILEDATA"))
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(10 << 20)
		for k, v := range r.MultipartForm.Value {
			params[k] = v[0]
		}
		for k := range r.MultipartForm.File {
			params[k] = "<file>"
		}
	} else {
		var raw map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)
		for k, v := range raw {
			if s, ok := v.(string); ok {
				params[k] = s
			} else {
				b, _ := json.Marshal(v)
				params[k] = string(b)
			}
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, tgCall{method, params})
	f.nextID++
	id := f.nextID
	f.mu.Unlock()

	var result any = true
	switch method {
	case "sendMessage", "sendPhoto", "sendDocument", "editMessageText", "editMessageReplyMarkup":
		chat := params["chat_id"]
		if chat == "" {
			chat = "0"
		}
		media := ""
		switch method { // real Telegram echoes the uploaded media back
		case "sendPhoto":
			media = `,"photo":[{"file_id":"ph","file_unique_id":"u","width":1,"height":1}]`
		case "sendDocument":
			media = `,"document":{"file_id":"doc","file_unique_id":"u"}`
		}
		result = json.RawMessage(fmt.Sprintf(`{"message_id":%d,"date":0,"chat":{"id":%s,"type":"private"},"from":{"id":%d,"is_bot":true,"username":"megabot"},"text":%q%s}`,
			id, chat, botID, params["text"], media))
	case "getFile":
		result = map[string]any{"file_id": params["file_id"], "file_path": "voice/file.oga"}
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeTelegram) take() []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// sent returns the text of sendMessage calls addressed to chat, ignoring chat actions.
func sentTo(calls []tgCall, chat int64) []tgCall {
	var out []tgCall
	for _, c := range calls {
		if c.Method != "sendChatAction" && c.Params["chat_id"] == fmt.Sprint(chat) {
			out = append(out, c)
		}
	}
	return out
}

// fakeLLM is an OpenAI-compatible server that records requests.
type fakeLLM struct {
	mu       sync.Mutex
	chats    [][]map[string]any // messages of each chat request
	models   []string
	answer   string
	transcr  string
	srv      *httptest.Server
	imageReq int
}

func newFakeLLM(t *testing.T, answer string) *fakeLLM {
	f := &fakeLLM{answer: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/chat/completions":
			var body struct {
				Model    string           `json:"model"`
				Messages []map[string]any `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.chats = append(f.chats, body.Messages)
			f.models = append(f.models, body.Model)
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": f.answer}}},
				"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 500},
			})
		case "/audio/transcriptions":
			json.NewEncoder(w).Encode(map[string]any{"text": f.transcr})
		case "/images/generations":
			f.imageReq++
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("PNG"))}}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) lastChat() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.chats) == 0 {
		return nil
	}
	return f.chats[len(f.chats)-1]
}

type harness struct {
	t     *testing.T
	tg    *Telegram
	api   *fakeTelegram
	oai   *fakeLLM
	local *fakeLLM
	upd   int
	msgID int
}

func newHarness(t *testing.T) *harness {
	api := newFakeTelegram(t)
	oai := newFakeLLM(t, "risposta openai")
	local := newFakeLLM(t, "risposta local")
	bot, err := telebot.NewBot(telebot.Settings{Token: tgToken, URL: api.srv.URL, Offline: true, Synchronous: true,
		OnError: func(err error, c telebot.Context) { t.Errorf("handler error: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	bot.Me = &telebot.User{ID: botID, IsBot: true, Username: "megabot"}
	db := database.NewDatabase(t.TempDir())
	db.CreateTables()
	t.Cleanup(db.Close)
	db.EnsureApproved(rootID, "root", "Root")
	cfg := config.Config{
		RootUserID: rootID,
		Providers: map[string]config.Provider{
			"openai": {Name: "openai", BaseURL: oai.srv.URL, APIKey: "k", ChatModel: "gpt-chat", SummaryModel: "gpt-sum", VisionModel: "gpt-vis", PriceIn: 1, PriceOut: 10},
			"local":  {Name: "local", BaseURL: local.srv.URL, APIKey: "k", ChatModel: "vllm/local", SummaryModel: "vllm/local", VisionModel: "vllm/local-vision"},
		},
		ProviderOrder:         []string{"openai", "local"},
		DefaultLLM:            "openai",
		TranscribeProvider:    "openai",
		TranscribeModel:       "whisper-1",
		TranscribePricePerMin: 0.006,
		ImageProvider:         "openai",
		ImageModel:            "gpt-image-1",
		ImagePrice:            0.04,
		DefaultUserBudget:     5,
		SummaryMaxChars:       10000,
		FetchFallback:         "none",
		HistoryMaxMessages:    20,
		MessageRetentionDays:  30,
	}
	tg := New(bot, db, cfg)
	tg.SetupHandlers()
	return &harness{t: t, tg: tg, api: api, oai: oai, local: local, msgID: 1}
}

func user(id int64, name string) *telebot.User {
	return &telebot.User{ID: id, Username: name, FirstName: name}
}

func (h *harness) send(from *telebot.User, m *telebot.Message) *telebot.Message {
	h.upd++
	h.msgID++
	m.ID = h.msgID
	m.Sender = from
	if m.Chat == nil {
		m.Chat = &telebot.Chat{ID: from.ID, Type: telebot.ChatPrivate}
	}
	h.tg.Bot.ProcessUpdate(telebot.Update{ID: h.upd, Message: m})
	return m
}

func (h *harness) text(from *telebot.User, text string) *telebot.Message {
	return h.send(from, &telebot.Message{Text: text})
}

func (h *harness) press(from *telebot.User, msg *telebot.Message, data string) {
	h.upd++
	h.tg.Bot.ProcessUpdate(telebot.Update{ID: h.upd, Callback: &telebot.Callback{ID: "cb", Sender: from, Message: msg, Data: data}})
}

func botMessage(id int, chat int64, text string) *telebot.Message {
	return &telebot.Message{ID: id, Text: text, Chat: &telebot.Chat{ID: chat, Type: telebot.ChatPrivate}, Sender: &telebot.User{ID: botID, IsBot: true}}
}

func mustContain(t *testing.T, calls []tgCall, want string) tgCall {
	t.Helper()
	for _, c := range calls {
		if strings.Contains(c.Params["text"], want) || strings.Contains(c.Params["reply_markup"], want) {
			return c
		}
	}
	t.Fatalf("no call containing %q in %+v", want, calls)
	return tgCall{}
}

func TestLoginApprovalFlow(t *testing.T) {
	h := newHarness(t)
	bob := user(bobID, "bob")

	h.text(bob, "ciao")
	mustContain(t, sentTo(h.api.take(), bobID), "/login")
	if len(h.oai.chats) != 0 {
		t.Fatal("unknown user must not reach the LLM")
	}

	h.text(bob, "/login")
	calls := h.api.take()
	mustContain(t, sentTo(calls, bobID), "Richiesta inviata")
	req := mustContain(t, sentTo(calls, rootID), "Richiesta di accesso")
	if !strings.Contains(req.Params["reply_markup"], "adm:ok:7") || !strings.Contains(req.Params["reply_markup"], "adm:no:7") {
		t.Fatalf("missing decision buttons: %s", req.Params["reply_markup"])
	}

	h.text(bob, "/login")
	mustContain(t, sentTo(h.api.take(), bobID), "in attesa")

	// only root can decide
	h.press(bob, botMessage(500, bobID, "x"), "adm:ok:7")
	if u, _ := h.tg.DB.GetUser(bobID); u.Status != database.StatusPending {
		t.Fatal("non-root approved a user")
	}
	h.api.take()

	h.press(user(rootID, "root"), botMessage(501, rootID, "🔔 Richiesta di accesso"), "adm:ok:7")
	calls = h.api.take()
	mustContain(t, sentTo(calls, bobID), "Accesso approvato")
	edit := mustContain(t, calls, "Approvato, budget $5/mese")
	if edit.Method != "editMessageText" {
		t.Fatalf("expected the request message to be edited, got %s", edit.Method)
	}
	u, _ := h.tg.DB.GetUser(bobID)
	if u.Status != database.StatusApproved || u.BudgetUSD == nil || *u.BudgetUSD != 5 {
		t.Fatalf("%+v", u)
	}

	h.text(bob, "ciao")
	mustContain(t, sentTo(h.api.take(), bobID), "risposta openai")
}

func TestRejectedUserIsSilent(t *testing.T) {
	h := newHarness(t)
	bob := user(bobID, "bob")
	h.text(bob, "/login")
	h.press(user(rootID, "root"), botMessage(501, rootID, "req"), "adm:no:7")
	mustContain(t, sentTo(h.api.take(), bobID), "rifiutata")
	h.text(bob, "/login")
	h.text(bob, "ciao")
	if got := sentTo(h.api.take(), bobID); len(got) != 0 {
		t.Fatalf("rejected user got replies: %+v", got)
	}
}

func TestConversationContinuesOnReply(t *testing.T) {
	h := newHarness(t)
	root := user(rootID, "root")
	h.text(root, "prima domanda")
	answer := sentTo(h.api.take(), rootID)
	if len(answer) != 1 {
		t.Fatalf("%+v", answer)
	}
	if n := len(h.oai.lastChat()); n != 1 {
		t.Fatalf("first turn should send 1 message, sent %d", n)
	}
	// reply to the bot's answer (its id is the one the fake API assigned)
	var botMsgID int
	fmt.Sscan(strings.TrimSpace(answerID(h, answer[0])), &botMsgID)
	h.send(root, &telebot.Message{Text: "e quindi?", ReplyTo: botMessage(botMsgID, rootID, "risposta openai")})
	msgs := h.oai.lastChat()
	if len(msgs) != 3 || msgs[0]["content"] != "prima domanda" || msgs[1]["role"] != "assistant" || msgs[2]["content"] != "e quindi?" {
		t.Fatalf("history not rebuilt: %+v", msgs)
	}
}

// answerID finds the message_id the fake server gave to a sendMessage call.
func answerID(h *harness, c tgCall) string {
	var id int
	for i := 1000; i < 1100; i++ {
		if m, _ := h.tg.DB.GetMessage(rootID, i); m != nil && m.Role == "assistant" && m.Content == c.Params["text"] {
			id = i
		}
	}
	return fmt.Sprint(id)
}

func TestBudgetBlocksPaidButNotFree(t *testing.T) {
	h := newHarness(t)
	bob := user(bobID, "bob")
	h.tg.DB.RequestAccess(bobID, "bob", "")
	zero := 0.0
	h.tg.DB.SetStatus(bobID, database.StatusApproved, &zero)

	h.text(bob, "ciao")
	mustContain(t, sentTo(h.api.take(), bobID), "budget mensile esaurito")
	if len(h.oai.chats) != 0 {
		t.Fatal("paid provider called over budget")
	}

	h.text(bob, "/llm local")
	mustContain(t, sentTo(h.api.take(), bobID), "Ora uso local")
	h.text(bob, "ciao")
	mustContain(t, sentTo(h.api.take(), bobID), "risposta local")

	// root has no budget
	h.text(user(rootID, "root"), "ciao")
	mustContain(t, sentTo(h.api.take(), rootID), "risposta openai")
}

func TestVoiceTranscriptionSummaryAndCost(t *testing.T) {
	h := newHarness(t)
	root := user(rootID, "root")
	h.oai.transcr = strings.Repeat("parola ", 120) // > 600 runes: gets the button
	h.send(root, &telebot.Message{Voice: &telebot.Voice{File: telebot.File{FileID: "v1"}, Duration: 120}})
	calls := sentTo(h.api.take(), rootID)
	tr := mustContain(t, calls, "parola parola")
	if !strings.Contains(tr.Params["reply_markup"], `"sum"`) {
		t.Fatalf("missing Riassumi button: %s", tr.Params["reply_markup"])
	}

	stats, _ := h.tg.DB.UsageStats(rootID)
	if len(stats) != 1 || stats[0].Kind != "audio" || stats[0].TotalUSD < 0.0119 || stats[0].TotalUSD > 0.0121 {
		t.Fatalf("2 minutes at $0.006/min should cost $0.012: %+v", stats)
	}

	// press Riassumi on the transcription message
	var trID int
	for i := 1000; i < 1100; i++ {
		if m, _ := h.tg.DB.GetMessage(rootID, i); m != nil && m.Role == "assistant" {
			trID = i
		}
	}
	h.press(root, botMessage(trID, rootID, tr.Params["text"]), "sum")
	calls = h.api.take()
	mustContain(t, sentTo(calls, rootID), "📝 risposta openai")
	if !strings.Contains(fmt.Sprint(h.oai.lastChat()[0]["content"]), "parola parola") {
		t.Fatalf("summary did not receive the transcript: %+v", h.oai.lastChat())
	}
	if h.oai.models[len(h.oai.models)-1] != "gpt-sum" {
		t.Fatalf("summary should use the summary model, used %s", h.oai.models[len(h.oai.models)-1])
	}
}

func TestPhotoWithAndWithoutCaption(t *testing.T) {
	h := newHarness(t)
	root := user(rootID, "root")
	h.send(root, &telebot.Message{Photo: &telebot.Photo{File: telebot.File{FileID: "p1"}}})
	ocr := fmt.Sprint(h.oai.lastChat()[0]["content"])
	if !strings.Contains(ocr, "Trascrivi") || !strings.Contains(ocr, "data:image/jpeg;base64,") {
		t.Fatalf("OCR prompt/image missing: %s", ocr)
	}
	h.send(root, &telebot.Message{Photo: &telebot.Photo{File: telebot.File{FileID: "p2"}}, Caption: "quanto costa?"})
	if q := fmt.Sprint(h.oai.lastChat()[0]["content"]); !strings.Contains(q, "quanto costa?") {
		t.Fatalf("caption should be the question: %s", q)
	}
	if h.oai.models[len(h.oai.models)-1] != "gpt-vis" {
		t.Fatal("photo should use the vision model")
	}
}

func TestImageAndAdminCommands(t *testing.T) {
	h := newHarness(t)
	root := user(rootID, "root")
	h.text(root, "/image un gatto")
	if h.oai.imageReq != 1 {
		t.Fatal("image not generated")
	}
	found := false
	for _, c := range h.api.take() {
		// telebot uploads photos with filename="", which Go parses as a plain field
		if c.Method == "sendPhoto" && (c.Params["photo"] == "<file>" || c.Params["photo"] == "PNG") {
			found = true
		}
	}
	if !found {
		t.Fatal("photo not sent")
	}

	h.tg.DB.RequestAccess(bobID, "bob", "")
	h.text(root, "/budget 7 2.5")
	mustContain(t, sentTo(h.api.take(), rootID), "$2.50/mese")
	h.text(root, "/users")
	mustContain(t, sentTo(h.api.take(), rootID), "🆔 7")
	h.text(root, "/revoke 7")
	mustContain(t, sentTo(h.api.take(), rootID), "revocato")
	if u, _ := h.tg.DB.GetUser(bobID); u.Status != database.StatusRejected {
		t.Fatal("revoke failed")
	}
	// admin commands are invisible to others
	h.tg.DB.SetStatus(bobID, database.StatusApproved, nil)
	h.text(user(bobID, "bob"), "/users")
	if got := sentTo(h.api.take(), bobID); len(got) != 0 {
		t.Fatalf("non-root got /users output: %+v", got)
	}
}
