package restart

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTheNewProcessGetsTheSameArgumentsAndEnvironmentPlusTheAddress(t *testing.T) {
	args := []string{"/usr/bin/jingclaw", "daemon", "--addr", "127.0.0.1:0"}
	env := []string{"A=1", "JINGCLAW_HOME=/h", AddrEnv + "=127.0.0.1:1", "B=2"}

	gotArgs, gotEnv := Spec(args, env, "127.0.0.1:41167")

	if strings.Join(gotArgs, " ") != strings.Join(args, " ") {
		t.Errorf("arguments are %v, want %v", gotArgs, args)
	}
	want := "A=1,JINGCLAW_HOME=/h,B=2," + AddrEnv + "=127.0.0.1:41167"
	if strings.Join(gotEnv, ",") != want {
		t.Errorf("environment is %v, want %s", gotEnv, want)
	}
	if len(env) != 4 || env[2] != AddrEnv+"=127.0.0.1:1" {
		t.Error("the caller's environment was changed")
	}

	if _, bare := Spec(args, env, ""); strings.Contains(strings.Join(bare, ","), AddrEnv) {
		t.Error("an address that was not given was handed over")
	}
}

func TestTheHandedOverAddressIsConsumedAndMustBeLoopback(t *testing.T) {
	for given, want := range map[string]string{
		"127.0.0.1:41167": "127.0.0.1:41167",
		"[::1]:8080":      "[::1]:8080",
		"localhost:9000":  "localhost:9000",
		"0.0.0.0:41167":   "",
		"10.1.2.3:80":     "",
		"127.0.0.1:0":     "",
		"127.0.0.1":       "",
		"nonsense":        "",
		"":                "",
	} {
		t.Setenv(AddrEnv, given)
		if got := TakeAddr(); got != want {
			t.Errorf("%q: got %q, want %q", given, got, want)
		}
		if value, present := os.LookupEnv(AddrEnv); present {
			t.Errorf("%q: the variable is still set to %q", given, value)
		}
	}
}

func TestOnlyTheFirstRequestStartsARestartAndItEndsTheContext(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	ctx, stop := NotifyContext(context.Background())
	defer stop()

	if Wanted() {
		t.Fatal("a restart is wanted before one was asked for")
	}
	if !Request("127.0.0.1:1") {
		t.Fatal("the first request was refused")
	}
	if Request("127.0.0.1:2") {
		t.Error("a second request was accepted while one was in progress")
	}
	if !Wanted() {
		t.Error("the restart is not wanted")
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("asking for a restart did not end the context")
	}
}
