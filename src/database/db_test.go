package database

import (
	"math"
	"strconv"
	"testing"
)

func newTestDB(t *testing.T) *Database {
	t.Helper()
	db := NewDatabase(t.TempDir())
	db.CreateTables()
	t.Cleanup(db.Close)
	return db
}

func ptr(f float64) *float64 { return &f }

func TestAccessRequestFlow(t *testing.T) {
	db := newTestDB(t)
	if u, _ := db.GetUser(7); u != nil {
		t.Fatal("user should not exist")
	}
	created, err := db.RequestAccess(7, "bob", "Bob")
	if err != nil || !created {
		t.Fatalf("first request: %v %v", created, err)
	}
	created, _ = db.RequestAccess(7, "bob", "Bob")
	if created {
		t.Fatal("second request must not create a new one")
	}
	u, _ := db.GetUser(7)
	if u.Status != StatusPending || u.BudgetUSD != nil {
		t.Fatalf("%+v", u)
	}
	if err := db.SetStatus(7, StatusApproved, ptr(5)); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(7)
	if u.Status != StatusApproved || u.BudgetUSD == nil || *u.BudgetUSD != 5 {
		t.Fatalf("%+v", u)
	}
	if ok, _ := db.SetBudget(7, nil); !ok {
		t.Fatal("budget update failed")
	}
	if u, _ = db.GetUser(7); u.BudgetUSD != nil {
		t.Fatal("budget should be unlimited")
	}
	if ok, _ := db.SetBudget(999, nil); ok {
		t.Fatal("unknown user must report false")
	}
	// a rejected user cannot ask again
	db.SetStatus(7, StatusRejected, nil)
	if created, _ = db.RequestAccess(7, "bob", "Bob"); created {
		t.Fatal("rejected user must not create a request")
	}
}

func TestEnsureApprovedAndLLM(t *testing.T) {
	db := newTestDB(t)
	db.EnsureApproved(1, "root", "Root")
	db.SetLLM(1, "mimir")
	db.EnsureApproved(1, "root2", "Root")
	u, _ := db.GetUser(1)
	if u.Status != StatusApproved || u.Username != "root2" || u.LLM != "mimir" {
		t.Fatalf("%+v", u)
	}
}

func TestUsageCostsAndStats(t *testing.T) {
	db := newTestDB(t)
	log := func(uid int64, kind, prov string, cost float64) {
		if err := db.LogUsage(UsageEntry{UserID: uid, UserName: "u", ChatID: 1, Kind: kind, Provider: prov, CostUSD: cost}); err != nil {
			t.Fatal(err)
		}
	}
	log(1, "audio", "openai", 0.01)
	log(1, "audio", "openai", 0.02)
	log(1, "text", "mimir", 0)
	log(2, "image", "openai", 0.04)
	db.db.Exec(`UPDATE usage SET created_at = '2000-01-01' WHERE kind = 'image'`) // last century: not this month

	spent, _ := db.MonthSpent(1)
	if math.Abs(spent-0.03) > 1e-9 {
		t.Fatalf("month spent %v", spent)
	}
	if s, _ := db.MonthSpent(2); s != 0 {
		t.Fatalf("old usage counted: %v", s)
	}
	all, err := db.UsageStats(0)
	if err != nil || len(all) != 3 {
		t.Fatalf("%v %+v", err, all)
	}
	if all[0].UserID != 1 || all[0].Kind != "audio" || all[0].Count != 2 {
		t.Fatalf("order %+v", all)
	}
	if all[2].TotalUSD != 0.04 || all[2].MonthUSD != 0 {
		t.Fatalf("user 2 %+v", all[2])
	}
	mine, _ := db.UsageStats(2)
	if len(mine) != 1 || mine[0].UserID != 2 {
		t.Fatalf("filter %+v", mine)
	}
}

func TestListUsers(t *testing.T) {
	db := newTestDB(t)
	db.EnsureApproved(1, "root", "")
	db.RequestAccess(2, "new", "")
	db.LogUsage(UsageEntry{UserID: 1, Kind: "text", CostUSD: 0.5})
	list, err := db.ListUsers()
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %+v", err, list)
	}
	if list[0].Status != StatusPending || list[1].MonthUSD != 0.5 || list[1].Requests != 1 {
		t.Fatalf("%+v", list)
	}
}

func TestThreadFollowsReplies(t *testing.T) {
	db := newTestDB(t)
	save := func(id int, role string, reply int) {
		if err := db.SaveMessage(5, Message{MessageID: id, Role: role, Content: role + strconv.Itoa(id), ReplyTo: reply}); err != nil {
			t.Fatal(err)
		}
	}
	save(1, "user", 0)
	save(2, "assistant", 1)
	save(3, "user", 2)
	save(4, "assistant", 3)
	save(9, "user", 0) // unrelated

	th, err := db.Thread(5, 4, 10)
	if err != nil || len(th) != 4 || th[0].MessageID != 1 || th[3].MessageID != 4 {
		t.Fatalf("%v %+v", err, th)
	}
	if th, _ = db.Thread(5, 4, 2); len(th) != 2 || th[0].MessageID != 3 {
		t.Fatalf("max not applied: %+v", th)
	}
	if th, _ = db.Thread(5, 42, 10); len(th) != 0 {
		t.Fatalf("unknown id: %+v", th)
	}
	// cycles must not loop forever
	db.SaveMessage(5, Message{MessageID: 1, Role: "user", Content: "x", ReplyTo: 4})
	if th, _ = db.Thread(5, 4, 100); len(th) != 4 {
		t.Fatalf("cycle: %+v", th)
	}
}

func TestPruneMessages(t *testing.T) {
	db := newTestDB(t)
	db.SaveMessage(1, Message{MessageID: 1, Role: "user", Content: "old"})
	db.SaveMessage(1, Message{MessageID: 2, Role: "user", Content: "new"})
	db.db.Exec(`UPDATE messages SET created_at = datetime('now', '-40 days') WHERE message_id = 1`)
	n, err := db.PruneMessages(30)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
}
