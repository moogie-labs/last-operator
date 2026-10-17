package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/moogie-labs/last-operator/internal/audit"
)

const (
	testSpecHash = "sha256:aaaa"
	testDigest   = "sha256:bbbb"
	otherDigest  = "sha256:cccc"
)

func TestImageIndexRoundTrips(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	x := newImageIndex(kube, ns)
	if err := x.load(ctx); err != nil {
		t.Fatalf("load with no ConfigMap: %v", err)
	}
	if _, ok := x.get(testSpecHash); ok {
		t.Fatal("an empty index returned an entry")
	}
	want := imageEntry{Digest: testDigest, BuildSeq: 7, LastUsed: time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)}
	if err := x.put(ctx, testSpecHash, want); err != nil {
		t.Fatal(err)
	}
	// A second put updates the existing ConfigMap.
	other := imageEntry{Digest: otherDigest, BuildSeq: 9, LastUsed: want.LastUsed}
	if err := x.put(ctx, "sha256:other", other); err != nil {
		t.Fatal(err)
	}
	fresh := newImageIndex(kube, ns)
	if err := fresh.load(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := fresh.get(testSpecHash); !ok || got != want {
		t.Fatalf("get = %v, %v; want %v", got, ok, want)
	}
	if err := fresh.drop(ctx, testSpecHash); err != nil {
		t.Fatal(err)
	}
	again := newImageIndex(kube, ns)
	if err := again.load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := again.get(testSpecHash); ok {
		t.Fatal("a dropped entry came back")
	}
	if got, ok := again.get("sha256:other"); !ok || got != other {
		t.Fatalf("the other entry = %v, %v", got, ok)
	}
}

func TestImageIndexFlushesLastUsedAtMostHourly(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	x := newImageIndex(kube, ns)
	t0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if err := x.put(ctx, testSpecHash, imageEntry{Digest: testDigest, BuildSeq: 1, LastUsed: t0}); err != nil {
		t.Fatal(err)
	}
	updates := func() int {
		n := 0
		for _, a := range kube.Actions() {
			if a.Matches("update", "configmaps") {
				n++
			}
		}
		return n
	}
	base := updates()
	// Touching an unknown spec changes nothing.
	x.touch("sha256:unknown", t0.Add(5*time.Minute))
	x.touch(testSpecHash, t0.Add(10*time.Minute))
	if err := x.flushUsed(ctx, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	x.touch(testSpecHash, t0.Add(20*time.Minute))
	if err := x.flushUsed(ctx, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := updates() - base; n != 0 {
		t.Fatalf("%d updates within the hour of the put, want 0", n)
	}
	x.touch(testSpecHash, t0.Add(61*time.Minute))
	if err := x.flushUsed(ctx, t0.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	x.touch(testSpecHash, t0.Add(90*time.Minute))
	if err := x.flushUsed(ctx, t0.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := updates() - base; n != 1 {
		t.Fatalf("%d updates, want 1", n)
	}
	x.touch(testSpecHash, t0.Add(125*time.Minute))
	if err := x.flushUsed(ctx, t0.Add(125*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := updates() - base; n != 2 {
		t.Fatalf("%d updates after another hour, want 2", n)
	}
	fresh := newImageIndex(kube, ns)
	if err := fresh.load(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := fresh.get(testSpecHash); !got.LastUsed.Equal(t0.Add(125 * time.Minute)) {
		t.Fatalf("persisted lastUsed = %v", got.LastUsed)
	}
}

func TestImageIndexRetriesAConflictOnce(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset()
	x := newImageIndex(kube, ns)
	if err := x.put(ctx, testSpecHash, imageEntry{Digest: testDigest, BuildSeq: 1}); err != nil {
		t.Fatal(err)
	}
	// Another writer changed the ConfigMap since load; the first update conflicts.
	conflicts := 0
	kube.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, imageIndexName, errors.New("stale"))
		}
		return false, nil, nil
	})
	if err := x.put(ctx, "sha256:other", imageEntry{Digest: otherDigest, BuildSeq: 2}); err != nil {
		t.Fatalf("put after one conflict: %v", err)
	}
	fresh := newImageIndex(kube, ns)
	if err := fresh.load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.get("sha256:other"); !ok {
		t.Fatal("the retried put was not saved")
	}
}

// writeBuildResult appends a sealed image.build.result entry and returns its seq.
func writeBuildResult(t *testing.T, env *testEnv, typ string, r buildResult) uint64 {
	t.Helper()
	e, err := env.g.writer.Load().Append(env.ctx, audit.Draft{Type: typ, Actor: "gateway", Intent: envKey, Stage: "Plan", Payload: r})
	if err != nil {
		t.Fatal(err)
	}
	return e.Seq
}

func succeeded() buildResult {
	return buildResult{SpecHash: testSpecHash, StartSeq: 1, Outcome: "succeeded", Digest: testDigest, DurationMs: 5, Log: "ok"}
}

func TestVerifyBuildAcceptsTheEntryThatBuiltTheDigest(t *testing.T) {
	env := newTestEnv(t)
	seq := writeBuildResult(t, env, audit.TypeImageBuildResult, succeeded())
	e := imageEntry{Digest: testDigest, BuildSeq: seq}
	if err := env.g.verifyBuild(env.ctx, testSpecHash, e); err != nil {
		t.Fatal(err)
	}
	// A pass is cached: it holds even when the entry later vanishes from the sink.
	env.sink.TamperReplace(audit.EntryKey(testChainID, seq), []byte("garbage"))
	if err := env.g.verifyBuild(env.ctx, testSpecHash, e); err != nil {
		t.Fatalf("cached pass: %v", err)
	}
	// The cache is per (spec, digest, seq): a different claim is checked afresh.
	if err := env.g.verifyBuild(env.ctx, testSpecHash, imageEntry{Digest: otherDigest, BuildSeq: seq}); err == nil {
		t.Fatal("a different digest passed from the cache")
	}
}

func TestVerifyBuildRejects(t *testing.T) {
	cases := map[string]func(t *testing.T, env *testEnv) (string, imageEntry){
		"a different digest": func(t *testing.T, env *testEnv) (string, imageEntry) {
			seq := writeBuildResult(t, env, audit.TypeImageBuildResult, succeeded())
			return testSpecHash, imageEntry{Digest: otherDigest, BuildSeq: seq}
		},
		"a different spec hash": func(t *testing.T, env *testEnv) (string, imageEntry) {
			seq := writeBuildResult(t, env, audit.TypeImageBuildResult, succeeded())
			return "sha256:dddd", imageEntry{Digest: testDigest, BuildSeq: seq}
		},
		"outcome failed": func(t *testing.T, env *testEnv) (string, imageEntry) {
			r := succeeded()
			r.Outcome = "failed"
			seq := writeBuildResult(t, env, audit.TypeImageBuildResult, r)
			return testSpecHash, imageEntry{Digest: testDigest, BuildSeq: seq}
		},
		"another entry type": func(t *testing.T, env *testEnv) (string, imageEntry) {
			seq := writeBuildResult(t, env, audit.TypeImageBuildStart, succeeded())
			return testSpecHash, imageEntry{Digest: testDigest, BuildSeq: seq}
		},
		"a missing seq": func(t *testing.T, env *testEnv) (string, imageEntry) {
			return testSpecHash, imageEntry{Digest: testDigest, BuildSeq: 9999}
		},
		"a bad signature": func(t *testing.T, env *testEnv) (string, imageEntry) {
			seq := writeBuildResult(t, env, audit.TypeImageBuildResult, succeeded())
			key := audit.EntryKey(testChainID, seq)
			body, err := env.sink.Get(env.ctx, key, "")
			if err != nil {
				t.Fatal(err)
			}
			e, err := audit.DecodeEntry(body)
			if err != nil {
				t.Fatal(err)
			}
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			if err := audit.NewSigner(other).Seal(&e); err != nil {
				t.Fatal(err)
			}
			forged, err := audit.Canonical(e)
			if err != nil {
				t.Fatal(err)
			}
			env.sink.TamperReplace(key, forged)
			return testSpecHash, imageEntry{Digest: testDigest, BuildSeq: seq}
		},
		"an empty digest": func(t *testing.T, env *testEnv) (string, imageEntry) {
			r := succeeded()
			r.Digest = ""
			seq := writeBuildResult(t, env, audit.TypeImageBuildResult, r)
			return testSpecHash, imageEntry{Digest: "", BuildSeq: seq}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			spec, e := setup(t, env)
			if err := env.g.verifyBuild(env.ctx, spec, e); err == nil {
				t.Fatal("verifyBuild accepted it")
			}
		})
	}
}
