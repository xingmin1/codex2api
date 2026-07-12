package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func newCompactAffinityTestHandler() (*Handler, *auth.Store, *auth.Account, *auth.Account) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		AffinityMode:   auth.AffinityModeBounded,
		MaxConcurrency: 2,
	})
	oldAccount := &auth.Account{
		DBID:              1,
		AccessToken:       "old-token",
		SchedulerPriority: 0,
	}
	newAccount := &auth.Account{
		DBID:              2,
		AccessToken:       "new-token",
		SchedulerPriority: 100,
	}
	store.AddAccount(oldAccount)
	store.AddAccount(newAccount)
	return NewHandler(store, nil, nil, nil), store, oldAccount, newAccount
}

func TestClearAffinityAfterSuccessfulCompactReleasesBoundedBinding(t *testing.T) {
	handler, store, oldAccount, newAccount := newCompactAffinityTestHandler()
	const affinityKey = "compact-session"
	store.BindSessionAffinity(affinityKey, oldAccount, "")

	handler.clearAffinityAfterSuccessfulCompact(affinityKey, oldAccount.ID(), true)

	selected, _ := store.NextForSession(affinityKey, 0, nil)
	if selected == nil {
		t.Fatal("expected an account after compact affinity is cleared")
	}
	if selected.ID() != newAccount.ID() {
		t.Fatalf("selected account = %d, want %d after bounded compact reset", selected.ID(), newAccount.ID())
	}
}

func TestClearAffinityAfterSuccessfulCompactKeepsBindingForNonCompact(t *testing.T) {
	handler, store, oldAccount, _ := newCompactAffinityTestHandler()
	const affinityKey = "ordinary-session"
	store.BindSessionAffinity(affinityKey, oldAccount, "")

	handler.clearAffinityAfterSuccessfulCompact(affinityKey, oldAccount.ID(), false)

	selected, _ := store.NextForSession(affinityKey, 0, nil)
	if selected == nil || selected.ID() != oldAccount.ID() {
		t.Fatalf("selected account = %v, want bound account %d for ordinary request", selected, oldAccount.ID())
	}
}

func TestClearAffinityAfterSuccessfulCompactDoesNotChangeStrictMode(t *testing.T) {
	settings := &database.SystemSettings{
		AffinityMode:   auth.AffinityModeStrict,
		MaxConcurrency: 2,
	}
	store := auth.NewStore(nil, nil, settings)
	oldAccount := &auth.Account{DBID: 1, AccessToken: "old-token"}
	newAccount := &auth.Account{DBID: 2, AccessToken: "new-token", SchedulerPriority: 100}
	store.AddAccount(oldAccount)
	store.AddAccount(newAccount)
	handler := NewHandler(store, nil, nil, nil)
	const affinityKey = "strict-session"
	store.BindSessionAffinity(affinityKey, oldAccount, "")

	handler.clearAffinityAfterSuccessfulCompact(affinityKey, oldAccount.ID(), true)

	selected, _ := store.NextForSession(affinityKey, 0, nil)
	if selected == nil || selected.ID() != oldAccount.ID() {
		t.Fatalf("selected account = %v, want strict bound account %d", selected, oldAccount.ID())
	}
}

func TestClearAffinityAfterSuccessfulCompactDoesNotDeleteNewBinding(t *testing.T) {
	handler, store, oldAccount, newAccount := newCompactAffinityTestHandler()
	const affinityKey = "concurrent-session"
	// 模拟 compact 完成前，另一个已完成的普通请求已经建立了新账号绑定。
	store.BindSessionAffinity(affinityKey, newAccount, "")

	handler.clearAffinityAfterSuccessfulCompact(affinityKey, oldAccount.ID(), true)

	selected, _ := store.NextForSession(affinityKey, 0, nil)
	if selected == nil || selected.ID() != newAccount.ID() {
		t.Fatalf("selected account = %v, want newer binding %d to survive", selected, newAccount.ID())
	}
}

func TestResponsesV2CompactionDetectionOnlyAcceptsDirectTrigger(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "top-level trigger",
			body: `{"input":[{"type":"compaction_trigger"}]}`,
			want: true,
		},
		{
			name: "input object trigger",
			body: `{"input":{"type":"compaction_trigger"}}`,
			want: true,
		},
		{
			name: "nested tool output is ordinary content",
			body: `{"input":[{"type":"function_call_output","output":{"type":"compaction_trigger"}}]}`,
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestBodyHasCompactionTrigger([]byte(test.body)); got != test.want {
				t.Fatalf("requestBodyHasCompactionTrigger() = %t, want %t", got, test.want)
			}
		})
	}
}
