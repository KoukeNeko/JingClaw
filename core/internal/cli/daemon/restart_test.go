package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/restart"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

func freePort(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	return probe.Addr().String()
}

func TestAListenerAfterARestartPrefersTheAddressItWasHandedAndFallsBackWhenItIsBusy(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handed := freePort(t)

	first, err := listen(handed, "127.0.0.1:0", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	if first.Addr().String() != handed {
		t.Errorf("listening on %s, want the handed-over %s", first.Addr(), handed)
	}

	was := handedPatience
	handedPatience = 200 * time.Millisecond
	defer func() { handedPatience = was }()

	// Busy: the configured address is used, as it would have been.
	for _, repair := range []bool{false, true} {
		second, err := listen(handed, "127.0.0.1:0", repair, logger)
		if err != nil {
			t.Fatalf("repair=%v with the handed-over address busy: %v", repair, err)
		}
		if second.Addr().String() == handed {
			t.Errorf("repair=%v: two listeners on %s", repair, handed)
		}
		_ = second.Close()
	}
	_ = first.Close()
}

func TestADaemonStartedByARestartServesOnTheHandedAddressAndConsumesTheVariable(t *testing.T) {
	root := isolate(t)
	handed := freePort(t)
	t.Setenv(restart.AddrEnv, handed)

	d := start(t, root)

	if got := d.file.BaseURL; got != "http://"+handed {
		t.Errorf("serving on %s, want http://%s", got, handed)
	}
	if value, present := os.LookupEnv(restart.AddrEnv); present {
		t.Errorf("the variable is still set to %q, and every program the daemon starts would inherit it", value)
	}
}

func restartCall(t *testing.T, d *running) (*controlv1.RestartDaemonResponse, error) {
	t.Helper()
	client := controlv1connect.NewConfigServiceClient(http.DefaultClient, d.file.BaseURL,
		connect.WithInterceptors(bearer(d.file.Token)))
	resp, err := client.RestartDaemon(context.Background(), connect.NewRequest(&controlv1.RestartDaemonRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestTheRestartIsAskedForWithAndWithoutAnAgentAndOnlyOnceAtATime(t *testing.T) {
	for name, seed := range map[string]string{
		"with an agent":    "",
		"without an agent": "[provider]\nbackend = \"nonsense\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			restart.Reset()
			t.Cleanup(restart.Reset)

			root := isolate(t)
			if seed != "" {
				seedDatabase(t, root, storage.DocumentConfig, seed)
			}
			d := start(t, root)

			if d.file.GatewayToken != "" {
				if got := d.call(controlv1connect.ConfigServiceName+"/RestartDaemon", d.file.GatewayToken); got != http.StatusForbidden {
					t.Errorf("a gateway credential asking for a restart got %d", got)
				}
			}

			resp, err := restartCall(t, d)
			if err != nil {
				t.Fatalf("RestartDaemon: %v", err)
			}
			if resp.RunsInFlight != 0 {
				t.Errorf("%d runs in flight on an idle daemon", resp.RunsInFlight)
			}
			if !restart.Wanted() {
				t.Error("the restart was answered and not asked for")
			}

			_, err = restartCall(t, d)
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("a second request while one is in progress: %v", err)
			}

			// Without the credential it is not reachable at all.
			anonymous, _ := http.NewRequest(http.MethodPost, d.file.BaseURL+"/"+controlv1connect.ConfigServiceName+"/RestartDaemon", bytes.NewReader([]byte("{}")))
			anonymous.Header.Set("Content-Type", "application/json")
			if got := d.do(anonymous); got != http.StatusUnauthorized {
				t.Errorf("a restart without a credential got %d", got)
			}
		})
	}
}

func TestAPasswordLogsInAndKeepsItsSessionAcrossARestartInRepairMode(t *testing.T) {
	root := isolate(t)
	seedDatabase(t, root, storage.DocumentConfig, "[provider]\nbackend = \"nonsense\"\n")
	d := start(t, root)

	browsers := controlv1connect.NewBrowserServiceClient(http.DefaultClient, d.file.BaseURL,
		connect.WithInterceptors(bearer(d.file.Token)))
	if _, err := browsers.SetDashboardPassword(context.Background(), connect.NewRequest(
		&controlv1.SetDashboardPasswordRequest{NewPassword: "a repair-mode password"})); err != nil {
		t.Fatal(err)
	}

	post := func(path, body string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, d.file.BaseURL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}
	resp, err := post("/ui/login", `{"password":"a repair-mode password"}`)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %v %v", resp, err)
	}
	var given struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&given)
	_ = resp.Body.Close()

	if got := d.call(controlv1connect.ConfigServiceName+"/GetAgentState", given.Token); got != http.StatusOK {
		t.Errorf("the session from a password gets %d in repair mode", got)
	}
	d.stop()

	again := start(t, root)
	if got := again.call(controlv1connect.ConfigServiceName+"/GetAgentState", given.Token); got != http.StatusOK {
		t.Errorf("after a restart the session gets %d", got)
	}
}
