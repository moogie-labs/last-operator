package audittest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moogie-labs/last-operator/internal/audit"
	"github.com/moogie-labs/last-operator/internal/audit/audittest"
)

var ctx = context.Background()

func TestMemSinkPutIsCreateOnly(t *testing.T) {
	m := audittest.NewMemSink()
	if err := m.Put(ctx, "k", []byte("a"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, "k", []byte("b"), time.Time{}); !errors.Is(err, audit.ErrExists) {
		t.Fatalf("second put = %v, want ErrExists", err)
	}
	got, err := m.Get(ctx, "k", "")
	if err != nil || string(got) != "a" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if _, err := m.Get(ctx, "missing", ""); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("missing get = %v, want ErrNotFound", err)
	}
}

func TestMemSinkListSortedWithStartAfter(t *testing.T) {
	m := audittest.NewMemSink()
	for _, k := range []string{"p/3", "p/1", "p/2", "q/1"} {
		_ = m.Put(ctx, k, []byte(k), time.Time{})
	}
	objs, err := m.List(ctx, "p/", "p/1")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Key != "p/2" || objs[1].Key != "p/3" {
		t.Fatalf("list = %+v", objs)
	}
}

func TestMemSinkTamperHelpersAreVisible(t *testing.T) {
	m := audittest.NewMemSink()
	_ = m.Put(ctx, "k", []byte("a"), time.Time{})
	m.TamperAddVersion("k", []byte("b"))
	m.TamperAddDeleteMarker("k")
	objs, _ := m.List(ctx, "", "")
	if len(objs) != 1 || len(objs[0].VersionIDs) != 2 || objs[0].DeleteMarkers != 1 {
		t.Fatalf("list = %+v", objs)
	}
	if got, _ := m.Get(ctx, "k", objs[0].VersionIDs[1]); string(got) != "b" {
		t.Fatalf("second version = %q", got)
	}
}

func TestMemSinkFailPuts(t *testing.T) {
	m := audittest.NewMemSink()
	m.SetFailPuts(true)
	if err := m.Put(ctx, "k", []byte("a"), time.Time{}); err == nil {
		t.Fatal("put succeeded while failing")
	}
}
