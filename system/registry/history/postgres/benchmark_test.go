package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/wippyai/runtime/api/payload"
	"github.com/wippyai/runtime/api/registry"
	"github.com/wippyai/runtime/internal/version"
	"go.uber.org/zap"
)

func BenchmarkDurableHistory(b *testing.B) {
	dsn := os.Getenv("WIPPY_POSTGRES_HISTORY_TEST_DSN")
	if dsn == "" {
		b.Fatal("WIPPY_POSTGRES_HISTORY_TEST_DSN is required")
	}
	schema := fmt.Sprintf("history_bench_%d", os.Getpid())
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		b.Fatal(err)
	}
	defer db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	history, err := NewPostgres(dsn, schema, zap.NewNop())
	if err != nil {
		b.Fatal(err)
	}
	defer history.Close()
	head, err := history.Head()
	if err != nil {
		b.Fatal(err)
	}
	changes := registry.ChangeSet{{Kind: registry.EntryUpdate, Entry: registry.Entry{ID: registry.NewID("test", "entry"), Kind: registry.EntryKind, Data: payload.NewString("value")}}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		next := version.FromParent(head, head.ID()+1)
		if err := history.Save(next, changes, true); err != nil {
			b.Fatal(err)
		}
		head = next
	}
}
