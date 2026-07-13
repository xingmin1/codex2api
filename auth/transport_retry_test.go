package auth

import (
	"testing"

	"github.com/codex2api/database"
)

func TestTransportSameAccountRetriesForAccount(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		TransportRetryPolicy:        "hybrid",
		TransportSameAccountRetries: 2,
	})
	account := &Account{DBID: 1, AccessToken: "token"}
	store.AddAccount(account)

	if got := store.TransportSameAccountRetriesForAccount(account); got != 2 {
		t.Fatalf("继承全局同号次数 = %d, want 2", got)
	}

	override := 0
	if !store.ApplyAccountTransportSameAccountRetries(account.DBID, true, &override) {
		t.Fatal("设置账号覆盖失败")
	}
	if got := store.TransportSameAccountRetriesForAccount(account); got != 0 {
		t.Fatalf("账号覆盖同号次数 = %d, want 0", got)
	}

	if !store.ApplyAccountTransportSameAccountRetries(account.DBID, true, nil) {
		t.Fatal("清除账号覆盖失败")
	}
	if got := store.TransportSameAccountRetriesForAccount(account); got != 2 {
		t.Fatalf("清除覆盖后同号次数 = %d, want 2", got)
	}
}

func TestCompactSameAccountRetriesForAccount(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:            2,
		CompactSameAccountRetries: 2,
	})
	account := &Account{DBID: 1, AccessToken: "token"}
	store.AddAccount(account)

	if got := store.CompactSameAccountRetriesForAccount(account); got != 2 {
		t.Fatalf("继承全局 compact 同号次数 = %d, want 2", got)
	}

	override := 0
	if !store.ApplyAccountCompactSameAccountRetries(account.DBID, true, &override) {
		t.Fatal("设置账号 compact 覆盖失败")
	}
	if got := store.CompactSameAccountRetriesForAccount(account); got != 0 {
		t.Fatalf("账号覆盖 compact 同号次数 = %d, want 0", got)
	}

	if !store.ApplyAccountCompactSameAccountRetries(account.DBID, true, nil) {
		t.Fatal("清除账号 compact 覆盖失败")
	}
	if got := store.CompactSameAccountRetriesForAccount(account); got != 2 {
		t.Fatalf("清除覆盖后 compact 同号次数 = %d, want 2", got)
	}
}

func TestTransportRetryDefaultsForNewStore(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)

	if got := store.GetTransportRetryPolicy(); got != "hybrid" {
		t.Fatalf("新 Store 传输重试策略 = %q, want hybrid", got)
	}
	if got := store.GetTransportSameAccountRetries(); got != 2 {
		t.Fatalf("新 Store 同号重试次数 = %d, want 2", got)
	}
	if got := store.GetCompactSameAccountRetries(); got != 2 {
		t.Fatalf("新 Store compact 同号重试次数 = %d, want 2", got)
	}
}
