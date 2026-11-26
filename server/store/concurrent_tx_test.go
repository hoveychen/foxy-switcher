package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentUpsertNoSQLiteBusy is the regression test for the production
// "save account: database is locked (5) (SQLITE_BUSY)" error. Upsert runs a
// read-then-write transaction; under the default DEFERRED BEGIN the read
// takes a snapshot and the later write must upgrade the lock. When another
// connection committed in between, SQLite returns SQLITE_BUSY immediately
// without honoring busy_timeout. Opening with _txlock=immediate takes the
// write lock at BEGIN, where busy_timeout does apply.
func TestConcurrentUpsertNoSQLiteBusy(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	const workers = 16
	const perWorker = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				a := &Account{
					Name:         fmt.Sprintf("u%d", w),
					Email:        fmt.Sprintf("u%d@example.com", w),
					AccessToken:  fmt.Sprintf("at-%d-%d", w, i),
					RefreshToken: "rt",
					ExpiresAt:    int64(i),
				}
				if err := st.Upsert(ctx, a); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upsert: %v", err)
	}
}
