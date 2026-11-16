package forkseed

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReapWithRunnerRetainsAgeAndDeleteChecks(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	old := RefPrefix + "old"
	young := RefPrefix + "young"
	var calls [][]string
	runner := func(ctx context.Context, root string, env []string, args ...string) (string, error) {
		if root != "fixture-workspace" || len(env) != 0 || ctx.Err() != nil {
			t.Fatalf("unexpected command context: root=%q env=%v err=%v", root, env, ctx.Err())
		}
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "for-each-ref":
			return fmt.Sprintf("%s %d\n%s %d\ninvalid-line\n%s invalid-time", old, now.Add(-2*time.Hour).Unix(), young, now.Add(-time.Minute).Unix(), RefPrefix+"invalid"), nil
		case "rev-parse":
			return "fixture-sha", nil
		case "update-ref":
			return "", nil
		default:
			t.Fatalf("unexpected command %v", args)
			return "", errors.New("unexpected")
		}
	}
	count, err := ReapWithRunner(context.Background(), "fixture-workspace", time.Hour, now, runner)
	want := [][]string{
		{"for-each-ref", "--format=%(refname) %(committerdate:unix)", RefPrefix},
		{"rev-parse", "--verify", "--quiet", old},
		{"update-ref", "-d", old},
	}
	if err != nil || count != 1 || !reflect.DeepEqual(calls, want) {
		t.Fatalf("count=%d err=%v calls=%v want=%v", count, err, calls, want)
	}
}

func TestReapWithRunnerRefusesMissingEffectAndUnownedRef(t *testing.T) {
	if _, err := ReapWithRunner(context.Background(), "", time.Hour, time.Now(), nil); err == nil {
		t.Fatal("nil runner must fail without a production fallback")
	}
	calls := 0
	runner := func(context.Context, string, []string, ...string) (string, error) {
		calls++
		return "refs/heads/main 1", nil
	}
	count, err := ReapWithRunner(context.Background(), "fixture", time.Hour, time.Now(), runner)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") || count != 0 || calls != 1 {
		t.Fatalf("unowned ref: count=%d calls=%d err=%v", count, calls, err)
	}
}

func TestReapWithRunnerPropagatesCancellationAndDeleteFailure(t *testing.T) {
	t.Run("cancelled read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		runner := func(ctx context.Context, _ string, _ []string, args ...string) (string, error) {
			calls++
			if args[0] != "for-each-ref" {
				t.Fatalf("unexpected delete %v", args)
			}
			return "", ctx.Err()
		}
		count, err := ReapWithRunner(ctx, "fixture", time.Hour, time.Now(), runner)
		if !errors.Is(err, context.Canceled) || count != 0 || calls != 1 {
			t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
		}
	})
	t.Run("failed delete preserves remaining refs", func(t *testing.T) {
		failure := errors.New("fixture write rejected")
		updates := 0
		runner := func(_ context.Context, _ string, _ []string, args ...string) (string, error) {
			switch args[0] {
			case "for-each-ref":
				return RefPrefix + "first 1\n" + RefPrefix + "second 1\n" + RefPrefix + "third 1", nil
			case "rev-parse":
				return "exists", nil
			case "update-ref":
				updates++
				if updates == 2 {
					return "", failure
				}
				return "", nil
			}
			t.Fatalf("unexpected command %v", args)
			return "", failure
		}
		count, err := ReapWithRunner(context.Background(), "fixture", time.Hour, time.Now(), runner)
		if !errors.Is(err, failure) || count != 1 || updates != 2 {
			t.Fatalf("count=%d updates=%d err=%v", count, updates, err)
		}
	})
}
