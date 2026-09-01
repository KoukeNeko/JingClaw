package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

// The gateway follows the daemon when it comes back somewhere else.
//
// The failure this closes was silent and lasted hours. The daemon publishes a
// fresh address every start, and a gateway that read that once and kept it was
// left dialling a port nobody answers — while still connected to the platform,
// still marking every message as seen, and answering none of them. From the
// chat room it looked exactly like an agent that had stopped talking.
func TestItFollowsTheDaemonToItsNewAddress(t *testing.T) {
	moved := make(chan string, 4)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moved <- "first:" + r.Header.Get("Authorization")
	}))
	defer first.Close()

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moved <- "second:" + r.Header.Get("Authorization")
	}))
	defer second.Close()

	path := filepath.Join(t.TempDir(), "daemon.json")
	publish(t, path, first.URL, "token-one")

	transport := &client.AtTheDaemon{Path: path, As: client.AsTheGateway, Base: http.DefaultTransport}
	client := &http.Client{Transport: transport}

	ask(t, client, "http://wherever.invalid/ping")
	if where := <-moved; where != "first:Bearer token-one" {
		t.Fatalf("the first request went to %q", where)
	}

	// The daemon restarts: new port, new credential.
	publish(t, path, second.URL, "token-two")

	ask(t, client, "http://wherever.invalid/ping")
	if where := <-moved; where != "second:Bearer token-two" {
		t.Errorf("after the daemon moved, the request went to %q", where)
	}
}

// A discovery file that cannot be read stops the request rather than sending
// it somewhere stale.
//
// Sending it to the last known address would be worse than failing: the reply
// would go to whatever now holds that port, carrying a credential meant for
// something else.
func TestARequestIsNotSentToAStaleAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")

	transport := &client.AtTheDaemon{Path: path, As: client.AsTheGateway, Base: http.DefaultTransport}
	client := &http.Client{Transport: transport}

	request, err := http.NewRequest(http.MethodGet, "http://wherever.invalid/ping", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = client.Do(request)
	if err == nil {
		t.Fatal("a request went out with no discovery file to say where")
	}
	// Which failure, not merely that one happened. Everything about this
	// transport fails when the daemon is unreachable, so a check that only
	// asked for an error would pass on any of them.
	if !strings.Contains(err.Error(), "where the daemon is") {
		t.Errorf("the failure does not say the address is unknown: %v", err)
	}
}

// The credential never travels to anywhere but the daemon that published it.
func TestTheCredentialGoesNowhereWithoutOne(t *testing.T) {
	// A server that would answer, so a refusal here cannot be a dial failure
	// wearing the right shape. Without one, dropping the check below would
	// look like it still worked: the request would fail to connect and the
	// check would call that a refusal.
	arrived := make(chan struct{}, 1)
	listening := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
	}))
	defer listening.Close()

	path := filepath.Join(t.TempDir(), "daemon.json")
	publish(t, path, listening.URL, "")

	transport := &client.AtTheDaemon{Path: path, As: client.AsTheGateway, Base: http.DefaultTransport}
	request, err := http.NewRequest(http.MethodGet, "http://wherever.invalid/ping", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if _, err := transport.RoundTrip(request); err == nil {
		t.Error("a request went out against a daemon that published no gateway credential")
	}
	select {
	case <-arrived:
		t.Error("the request reached the daemon with no credential to carry")
	default:
	}
}

func publish(t *testing.T, path, baseURL, token string) {
	t.Helper()

	encoded, err := json.Marshal(discovery.File{
		PID: 1, BaseURL: baseURL, Token: "local", GatewayToken: token,
		ProtocolVersion: discovery.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func ask(t *testing.T, client *http.Client, url string) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	_ = response.Body.Close()
}
