package proxy

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestSelectionCancellationStopsInitialScan(t *testing.T) {
	for _, engine := range []string{"legacy", "indexed"} {
		for _, cancelBefore := range []bool{true, false} {
			name := engine + "/during_scan"
			if cancelBefore {
				name = engine + "/before_scan"
			}
			t.Run(name, func(t *testing.T) {
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: engine})
				t.Cleanup(store.Stop)
				for id := int64(1); id <= 32; id++ {
					store.AddAccount(&auth.Account{DBID: id, AccessToken: "test-token", Status: auth.StatusReady})
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if cancelBefore {
					cancel()
				}
				calls := 0
				filter := func(*auth.Account) bool {
					calls++
					cancel()
					return false
				}
				h := &Handler{store: store}
				account, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), filter, false, auth.DispatchPolicyStandard)
				if account != nil {
					store.Release(account)
					t.Fatal("selected an account after cancellation")
				}
				wantCalls := 1
				if cancelBefore {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Errorf("expensive filter calls = %d, want %d", calls, wantCalls)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("selection error = %v, want context.Canceled", err)
				}
				if got := store.GetSchedulerMetrics().WaitStarted; got != 0 {
					t.Errorf("started %d queue waits after cancellation", got)
				}
			})
		}
	}
}

func TestSelectionBudgetIncludesInitialScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "legacy"})
		defer store.Stop()
		for id := int64(1); id <= 4; id++ {
			store.AddAccount(&auth.Account{DBID: id, AccessToken: "test-token", Status: auth.StatusReady})
		}
		calls := 0
		filter := func(*auth.Account) bool {
			calls++
			// A synchronous filter cannot be preempted, but after it returns
			// the expired budget must stop all subsequent expensive checks.
			time.Sleep(16 * time.Second)
			return false
		}
		h := &Handler{store: store}
		account, _, _, err := h.nextRetryAccountWithGuard(context.Background(), "", 0, newRetryAccountExclusions(), filter, false, auth.DispatchPolicyStandard)
		if account != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
			t.Fatalf("selection returned account=%v, error=%v, filter calls=%d", account, err, calls)
		}
		if got := store.GetSchedulerMetrics().WaitStarted; got != 0 {
			t.Fatalf("expired initial scan started %d queue waits", got)
		}
	})
}

func TestSelectionSuccessPreservesRequestLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
		defer store.Stop()
		account := &auth.Account{DBID: 1, AccessToken: "test-token", Status: auth.StatusReady}
		store.AddAccount(account)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h := &Handler{store: store}
		got, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), nil, false, auth.DispatchPolicyStandard)
		if err != nil || got != account {
			t.Fatalf("selection = %v, %v", got, err)
		}
		defer store.Release(got)
		time.Sleep(31 * time.Second)
		if ctx.Err() != nil || got.GetActiveRequests() != 1 {
			t.Fatalf("selection timeout affected upstream lifetime: context=%v, active=%d", ctx.Err(), got.GetActiveRequests())
		}
	})
}

func TestSelectionDeadlineStopsPoolWait(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	h := &Handler{store: store}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	account, _, _, err := h.nextRetryAccountWithGuard(ctx, "", 0, newRetryAccountExclusions(), nil, false, auth.DispatchPolicyStandard)
	if account != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("selection returned account=%v, error=%v", account, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("pool wait ignored request deadline: %s", elapsed)
	}
	if got := store.GetSchedulerMetrics().Waiters; got != 0 {
		t.Fatalf("leaked %d queue waiters", got)
	}
}
