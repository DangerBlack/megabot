package database

import (
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strconv"

	_ "github.com/mattn/go-sqlite3"
)

type Database struct {
	db *sql.DB
}

func NewDatabase(path string) *Database {
	db, err := sql.Open("sqlite3", filepath.Join(path, "megabot.sqlite")+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		log.Fatal("failed to open database: ", err)
	}
	db.SetMaxOpenConns(1)
	return &Database{db}
}

func (d *Database) Close() {
	d.db.Close()
	log.Default().Println("database connection closed")
}

func (d *Database) CreateTables() {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			budget_usd REAL,
			llm TEXT NOT NULL DEFAULT '',
			requested_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			decided_at TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS usage (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			user_name TEXT NOT NULL DEFAULT '',
			chat_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			audio_seconds INTEGER NOT NULL DEFAULT 0,
			images INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_user ON usage(user_id, created_at);`,
		`CREATE TABLE IF NOT EXISTS messages (
			chat_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			reply_to INTEGER,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (chat_id, message_id)
		);`,
	}
	for _, q := range queries {
		if _, err := d.db.Exec(q); err != nil {
			log.Fatal(err)
		}
	}
}

// ---------- users ----------

const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

type User struct {
	ID        int64
	Username  string
	Name      string
	Status    string
	BudgetUSD *float64 // nil = no limit
	LLM       string   // "" = default provider
}

func (d *Database) GetUser(id int64) (*User, error) {
	u := &User{}
	var budget sql.NullFloat64
	err := d.db.QueryRow(`SELECT user_id, username, name, status, budget_usd, llm FROM users WHERE user_id = ?`, id).
		Scan(&u.ID, &u.Username, &u.Name, &u.Status, &budget, &u.LLM)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if budget.Valid {
		u.BudgetUSD = &budget.Float64
	}
	return u, nil
}

// EnsureApproved creates the user as approved without a budget (used for root),
// refreshing username/name if the row exists.
func (d *Database) EnsureApproved(id int64, username, name string) error {
	_, err := d.db.Exec(`
		INSERT INTO users (user_id, username, name, status, decided_at) VALUES (?, ?, ?, 'approved', CURRENT_TIMESTAMP)
		ON CONFLICT(user_id) DO UPDATE SET username = excluded.username, name = excluded.name, status = 'approved', budget_usd = NULL`,
		id, username, name)
	return err
}

// RequestAccess records a pending request. It returns false if the user
// already exists (pending, approved or rejected), so no new request is made.
func (d *Database) RequestAccess(id int64, username, name string) (bool, error) {
	res, err := d.db.Exec(`INSERT INTO users (user_id, username, name, status) VALUES (?, ?, ?, 'pending')
		ON CONFLICT(user_id) DO NOTHING`, id, username, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (d *Database) SetStatus(id int64, status string, budget *float64) error {
	_, err := d.db.Exec(`UPDATE users SET status = ?, budget_usd = ?, decided_at = CURRENT_TIMESTAMP WHERE user_id = ?`,
		status, nullFloat(budget), id)
	return err
}

func (d *Database) SetBudget(id int64, budget *float64) (bool, error) {
	res, err := d.db.Exec(`UPDATE users SET budget_usd = ? WHERE user_id = ?`, nullFloat(budget), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (d *Database) SetLLM(id int64, llm string) error {
	_, err := d.db.Exec(`UPDATE users SET llm = ? WHERE user_id = ?`, llm, id)
	return err
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

type UserSummary struct {
	User
	MonthUSD float64
	Requests int
}

func (d *Database) ListUsers() ([]UserSummary, error) {
	rows, err := d.db.Query(`
		SELECT u.user_id, u.username, u.name, u.status, u.budget_usd, u.llm,
		       COALESCE(SUM(CASE WHEN strftime('%Y-%m', g.created_at) = strftime('%Y-%m', 'now') THEN g.cost_usd END), 0),
		       COUNT(g.id)
		FROM users u LEFT JOIN usage g ON g.user_id = u.user_id
		GROUP BY u.user_id
		ORDER BY CASE u.status WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 ELSE 2 END, COUNT(g.id) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var s UserSummary
		var budget sql.NullFloat64
		if err := rows.Scan(&s.ID, &s.Username, &s.Name, &s.Status, &budget, &s.LLM, &s.MonthUSD, &s.Requests); err != nil {
			return nil, err
		}
		if budget.Valid {
			s.BudgetUSD = &budget.Float64
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------- usage ----------

type UsageEntry struct {
	UserID       int64
	UserName     string
	ChatID       int64
	Kind         string
	Provider     string
	Model        string
	TokensIn     int
	TokensOut    int
	AudioSeconds int
	Images       int
	CostUSD      float64
}

func (d *Database) LogUsage(e UsageEntry) error {
	_, err := d.db.Exec(`INSERT INTO usage (user_id, user_name, chat_id, kind, provider, model, tokens_in, tokens_out, audio_seconds, images, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.UserID, e.UserName, e.ChatID, e.Kind, e.Provider, e.Model, e.TokensIn, e.TokensOut, e.AudioSeconds, e.Images, e.CostUSD)
	return err
}

// MonthSpent is the USD spent by a user in the current calendar month (UTC).
func (d *Database) MonthSpent(userID int64) (float64, error) {
	var v float64
	err := d.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM usage
		WHERE user_id = ? AND strftime('%Y-%m', created_at) = strftime('%Y-%m', 'now')`, userID).Scan(&v)
	return v, err
}

type UsageRow struct {
	UserID   int64
	UserName string
	Kind     string
	Provider string
	Count    int
	MonthUSD float64
	TotalUSD float64
}

// UsageStats returns counts and costs per user, kind and provider; userID 0 = everyone.
func (d *Database) UsageStats(userID int64) ([]UsageRow, error) {
	rows, err := d.db.Query(`
		SELECT user_id, MAX(user_name), kind, provider, COUNT(*),
		       COALESCE(SUM(CASE WHEN strftime('%Y-%m', created_at) = strftime('%Y-%m', 'now') THEN cost_usd END), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM usage
		WHERE ? = 0 OR user_id = ?
		GROUP BY user_id, kind, provider
		ORDER BY SUM(COUNT(*)) OVER (PARTITION BY user_id) DESC, user_id, COUNT(*) DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.UserID, &r.UserName, &r.Kind, &r.Provider, &r.Count, &r.MonthUSD, &r.TotalUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- conversation messages ----------

type Message struct {
	MessageID int
	Role      string // user | assistant
	Content   string
	ReplyTo   int // 0 = none
}

func (d *Database) SaveMessage(chatID int64, m Message) error {
	var replyTo any
	if m.ReplyTo != 0 {
		replyTo = m.ReplyTo
	}
	_, err := d.db.Exec(`INSERT OR REPLACE INTO messages (chat_id, message_id, role, content, reply_to) VALUES (?, ?, ?, ?, ?)`,
		chatID, m.MessageID, m.Role, m.Content, replyTo)
	return err
}

func (d *Database) GetMessage(chatID int64, messageID int) (*Message, error) {
	m := &Message{}
	var replyTo sql.NullInt64
	err := d.db.QueryRow(`SELECT message_id, role, content, reply_to FROM messages WHERE chat_id = ? AND message_id = ?`,
		chatID, messageID).Scan(&m.MessageID, &m.Role, &m.Content, &replyTo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.ReplyTo = int(replyTo.Int64)
	return m, nil
}

// Thread follows reply_to links back from messageID and returns up to max
// messages, oldest first.
func (d *Database) Thread(chatID int64, messageID, max int) ([]Message, error) {
	var chain []Message
	seen := map[int]bool{}
	for id := messageID; id != 0 && len(chain) < max && !seen[id]; {
		seen[id] = true
		m, err := d.GetMessage(chatID, id)
		if err != nil {
			return nil, err
		}
		if m == nil {
			break
		}
		chain = append(chain, *m)
		id = m.ReplyTo
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// PruneMessages deletes conversation messages older than days.
func (d *Database) PruneMessages(days int) (int64, error) {
	res, err := d.db.Exec(`DELETE FROM messages WHERE created_at < datetime('now', ?)`, "-"+strconv.Itoa(days)+" days")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
