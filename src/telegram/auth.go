package telegram

import (
	"errors"
	"fmt"
	"log"
	"megabot/src/database"
	"strconv"
	"strings"

	"gopkg.in/telebot.v3"
)

// commandOf returns the lowercased command name of text ("/login@bot x" → "login"), or "".
func commandOf(text string) string {
	m := cmdRx.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

func (t *Telegram) Login(c telebot.Context) error {
	s := c.Sender()
	if t.isRoot(c) {
		return c.Send("👑 Sei l'amministratore, hai già accesso a tutto.")
	}
	if u := userOf(c); u != nil {
		switch u.Status {
		case database.StatusApproved:
			return c.Send("✅ Sei già autorizzato. Scrivi /help.")
		case database.StatusPending:
			return c.Send(lockedText(u))
		default: // rejected: stay silent
			return nil
		}
	}
	created, err := t.DB.RequestAccess(s.ID, s.Username, fullName(s))
	if err != nil {
		return t.replyError(c, err)
	}
	if !created {
		return nil
	}
	text := fmt.Sprintf("🔔 Richiesta di accesso\n\n👤 %s\n🆔 %d", displayName(s), s.ID)
	if _, err := t.Bot.Send(telebot.ChatID(t.Cfg.RootUserID), text, t.decisionKeyboard(s.ID)); err != nil {
		log.Printf("login: cannot notify root: %v", err)
		return c.Send("⚠️ Non riesco a contattare l'amministratore, riprova più tardi.")
	}
	return c.Send("📨 Richiesta inviata! Ti scrivo appena l'amministratore risponde.")
}

func (t *Telegram) decisionKeyboard(userID int64) *telebot.ReplyMarkup {
	id := strconv.FormatInt(userID, 10)
	return &telebot.ReplyMarkup{InlineKeyboard: [][]telebot.InlineButton{
		{
			{Text: fmt.Sprintf("✅ Accetta ($%s/mese)", money(t.Cfg.DefaultUserBudget)), Data: "adm:ok:" + id},
			{Text: "♾ Senza limite", Data: "adm:inf:" + id},
		},
		{{Text: "❌ Rifiuta", Data: "adm:no:" + id}},
	}}
}

// adminDecision handles the Accept / Unlimited / Reject buttons sent to root.
func (t *Telegram) adminDecision(c telebot.Context, data string) error {
	if !t.isRoot(c) {
		return c.Respond(&telebot.CallbackResponse{Text: "Solo l'amministratore"})
	}
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return c.Respond()
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return c.Respond()
	}
	u, err := t.DB.GetUser(id)
	if err != nil || u == nil {
		return c.Respond(&telebot.CallbackResponse{Text: "Utente non trovato"})
	}

	var outcome, notice string
	switch parts[1] {
	case "ok":
		b := t.Cfg.DefaultUserBudget
		err = t.DB.SetStatus(id, database.StatusApproved, &b)
		outcome = fmt.Sprintf("✅ Approvato, budget $%s/mese", money(b))
		notice = "✅ Accesso approvato! Scrivi /help per vedere cosa so fare."
	case "inf":
		err = t.DB.SetStatus(id, database.StatusApproved, nil)
		outcome = "✅ Approvato senza limite di spesa"
		notice = "✅ Accesso approvato! Scrivi /help per vedere cosa so fare."
	case "no":
		err = t.DB.SetStatus(id, database.StatusRejected, nil)
		outcome = "❌ Rifiutato"
		notice = "❌ La tua richiesta di accesso è stata rifiutata."
	default:
		return c.Respond()
	}
	if err != nil {
		return c.Respond(&telebot.CallbackResponse{Text: "Errore: " + err.Error()})
	}
	cb := c.Callback()
	// editing the text without a markup also removes the buttons
	if _, err := t.Bot.Edit(cb.Message, cb.Message.Text+"\n\n→ "+outcome); err != nil {
		log.Printf("admin: edit: %v", err)
	}
	if _, err := t.Bot.Send(telebot.ChatID(id), notice); err != nil {
		log.Printf("admin: notify %d: %v", id, err)
	}
	return c.Respond(&telebot.CallbackResponse{Text: outcome})
}

func (t *Telegram) Users(c telebot.Context) error {
	if !t.isRoot(c) {
		return nil
	}
	list, err := t.DB.ListUsers()
	if err != nil {
		return t.replyError(c, err)
	}
	_, err = t.replyLong(c, nil, formatUsers(list, t.Cfg.RootUserID, t.Cfg.DefaultLLM), nil)
	return err
}

func formatUsers(list []database.UserSummary, rootID int64, defaultLLM string) string {
	if len(list) == 0 {
		return "Nessun utente."
	}
	icon := map[string]string{database.StatusPending: "⏳", database.StatusApproved: "✅", database.StatusRejected: "⛔"}
	var b strings.Builder
	for _, u := range list {
		name := u.Name
		if u.Username != "" {
			name = strings.TrimSpace(name + " @" + u.Username)
		}
		if u.ID == rootID {
			name += " 👑"
		}
		budget := "∞"
		if u.BudgetUSD != nil {
			budget = "$" + money(*u.BudgetUSD)
		}
		llm := u.LLM
		if llm == "" {
			llm = defaultLLM
		}
		fmt.Fprintf(&b, "%s %s\n   🆔 %d · mese $%s / %s · %d richieste · %s\n",
			icon[u.Status], name, u.ID, money(u.MonthUSD), budget, u.Requests, llm)
	}
	return strings.TrimSpace(b.String())
}

func (t *Telegram) Revoke(c telebot.Context) error {
	if !t.isRoot(c) {
		return nil
	}
	id, err := parseID(c.Args())
	if err != nil {
		return c.Reply("Uso: /revoke <id>")
	}
	if id == t.Cfg.RootUserID {
		return c.Reply("Non puoi revocare te stesso 🙂")
	}
	u, err := t.DB.GetUser(id)
	if err != nil || u == nil {
		return c.Reply("Utente non trovato.")
	}
	if err := t.DB.SetStatus(id, database.StatusRejected, nil); err != nil {
		return t.replyError(c, err)
	}
	return c.Reply(fmt.Sprintf("⛔ Accesso revocato a %d.", id))
}

func (t *Telegram) Budget(c telebot.Context) error {
	if !t.isRoot(c) {
		return nil
	}
	args := c.Args()
	if len(args) != 2 {
		return c.Reply("Uso: /budget <id> <usd|none>")
	}
	id, err := parseID(args[:1])
	if err != nil {
		return c.Reply("ID non valido.")
	}
	budget, err := parseBudget(args[1])
	if err != nil {
		return c.Reply(err.Error())
	}
	ok, err := t.DB.SetBudget(id, budget)
	if err != nil {
		return t.replyError(c, err)
	}
	if !ok {
		return c.Reply("Utente non trovato.")
	}
	if budget == nil {
		return c.Reply(fmt.Sprintf("♾ %d ora non ha limiti di spesa.", id))
	}
	return c.Reply(fmt.Sprintf("💰 Budget di %d: $%s/mese.", id, money(*budget)))
}

func parseID(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, errors.New("missing id")
	}
	return strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
}

// parseBudget accepts "5", "2.5", "2,5", "$5" or "none"/"∞" (no limit).
func parseBudget(s string) (*float64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	switch strings.ToLower(s) {
	case "none", "nessuno", "inf", "∞", "unlimited":
		return nil, nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || v < 0 {
		return nil, fmt.Errorf("budget non valido: %q (usa un numero in dollari o none)", s)
	}
	return &v, nil
}

func money(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return strings.TrimSuffix(s, ".00")
}

func (t *Telegram) SetLLM(c telebot.Context) error {
	u := userOf(c)
	current := t.llmFor(u)
	args := c.Args()
	if len(args) == 0 {
		var b strings.Builder
		b.WriteString("🧠 Modelli disponibili:\n")
		for _, name := range t.Cfg.ProviderOrder {
			l := t.LLMs[name]
			mark := "  "
			if l == current {
				mark = "✅"
			}
			cost := "gratis"
			if l.Paid() {
				cost = fmt.Sprintf("$%s/$%s per M token", money(l.PriceIn), money(l.PriceOut))
			}
			fmt.Fprintf(&b, "%s %s · %s (%s)\n", mark, name, l.ChatModel, cost)
		}
		b.WriteString("\nUsa /llm <nome> per cambiare. Trascrizioni e immagini usano sempre il loro provider.")
		return c.Reply(b.String())
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	if _, ok := t.LLMs[name]; !ok {
		return c.Reply(fmt.Sprintf("Non conosco %q. Disponibili: %s", name, strings.Join(t.Cfg.ProviderOrder, ", ")))
	}
	if err := t.DB.SetLLM(u.ID, name); err != nil {
		return t.replyError(c, err)
	}
	return c.Reply(fmt.Sprintf("🧠 Ora uso %s (%s).", name, t.LLMs[name].ChatModel))
}

func (t *Telegram) Usage(c telebot.Context) error {
	var filter int64
	if !t.isRoot(c) {
		filter = c.Sender().ID
	}
	rows, err := t.DB.UsageStats(filter)
	if err != nil {
		return t.replyError(c, err)
	}
	_, err = t.replyLong(c, nil, formatUsage(rows), nil)
	return err
}

func formatUsage(rows []database.UsageRow) string {
	if len(rows) == 0 {
		return "Nessuna richiesta registrata."
	}
	type tot struct{ month, all float64 }
	totals := map[int64]*tot{}
	for _, r := range rows {
		if totals[r.UserID] == nil {
			totals[r.UserID] = &tot{}
		}
		totals[r.UserID].month += r.MonthUSD
		totals[r.UserID].all += r.TotalUSD
	}
	var b strings.Builder
	var current int64 = -1
	for _, r := range rows {
		if r.UserID != current {
			current = r.UserID
			t := totals[r.UserID]
			fmt.Fprintf(&b, "\n👤 %s · mese $%s · totale $%s\n", r.UserName, money(t.month), money(t.all))
		}
		prov := ""
		if r.Provider != "" {
			prov = " (" + r.Provider + ")"
		}
		fmt.Fprintf(&b, "  %s%s: %d\n", r.Kind, prov, r.Count)
	}
	return strings.TrimSpace(b.String())
}
