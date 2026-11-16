package inferenceguard

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeRegistryTracksExactTurnOwnerAcrossHostTransfer(t *testing.T) {
	ctx := context.Background()
	p := testPolicy()
	p.NativeAccountSHA256 = AccountFingerprint("chatgpt", "fixture@example.invalid")
	now := time.Now()
	path := filepath.Join(t.TempDir(), "authority.db")
	open := func() *Gate {
		g, e := Open(path, func() (Policy, error) { return p, nil }, func() time.Time { return now })
		if e != nil {
			t.Fatal(e)
		}
		return g
	}
	g := open()
	defer func() { _ = g.Close() }()
	if err := g.RegisterNativeThread(ctx, "fork-worker", "native-thread"); !errors.Is(err, ErrPolicy) {
		t.Fatal("unverified account registered thread", err)
	}
	if err := g.VerifyAccount(ctx, p.NativeAccountSHA256); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"fork-worker", "worker", "takeover"} {
		if err := g.RegisterNativeThread(ctx, session, "native-thread"); err != nil {
			t.Fatal(err)
		}
	}
	requestFor := func(id, session string) Request {
		r := request(t, g, id, "project", "implementation", "sol", "medium")
		r.Session, r.NativeThreadID = session, "native-thread"
		return r
	}
	first := requestFor("first", "worker")
	if err := g.Dispatch(ctx, first, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, "first", "root-one"); err != nil {
		t.Fatal(err)
	}
	owner, err := g.ResolveReceiptSession(ctx, "native-thread", "root-one")
	if err != nil || owner != "worker" {
		t.Fatalf("%s %v", owner, err)
	}
	if _, err := g.ResolveReceiptSession(ctx, "different-thread", "root-one"); !errors.Is(err, ErrUnboundNativeTurn) {
		t.Fatal("copied/foreign native history acquired ownership", err)
	}
	second := requestFor("second", "takeover")
	if err := g.Dispatch(ctx, second, func() error { t.Fatal("two hosts wrote one native thread"); return nil }); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if err := g.Complete(ctx, "worker", "root-one", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.Dispatch(ctx, second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.Bind(ctx, "second", "root-two"); err != nil {
		t.Fatal(err)
	}
	_ = g.Close()
	g = open()
	for turn, want := range map[string]string{"root-one": "worker", "root-two": "takeover"} {
		owner, err := g.ResolveReceiptSession(ctx, "native-thread", turn)
		if err != nil || owner != want {
			t.Fatalf("turn=%s owner=%s want=%s err=%v", turn, owner, want, err)
		}
	}
	threads, err := g.NativeThreads(ctx)
	if err != nil || len(threads) != 3 {
		t.Fatalf("%+v %v", threads, err)
	}
}
