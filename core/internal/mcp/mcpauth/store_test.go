package mcpauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func opened(t *testing.T) (*Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), DirName)
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store, dir
}

func aSession() (*oauth2.Config, *oauth2.Token) {
	return &oauth2.Config{
			ClientID: "registered-at-login",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://as.example/authorize",
				TokenURL: "https://as.example/token",
			},
			Scopes: []string{"mcp:read"},
		}, &oauth2.Token{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour).Round(time.Second),
		}
}

func TestASessionComesBackWholeAndNotJustItsRefreshToken(t *testing.T) {
	store, _ := opened(t)
	config, token := aSession()

	if err := store.Save("books", config, token); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.Load("books")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The client was registered during the flow that produced this token, so
	// it exists nowhere else. A store that kept only the refresh token would
	// work exactly until the first refresh needed a client id.
	if got.Config == nil || got.Config.ClientID != "registered-at-login" {
		t.Errorf("the client this was obtained with is gone: %+v", got.Config)
	}
	if got.Config.Endpoint.TokenURL != "https://as.example/token" {
		t.Errorf("the endpoint to refresh against is gone: %+v", got.Config)
	}
	if got.Token.RefreshToken != "refresh-1" {
		t.Errorf("refresh token: got %q", got.Token.RefreshToken)
	}
	if got.Token.AccessToken != "access-1" {
		t.Errorf("access token: got %q", got.Token.AccessToken)
	}
}

func TestNobodySignedInReadsAsNobodySignedIn(t *testing.T) {
	store, _ := opened(t)

	_, err := store.Load("never-used")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestWhatIsStoredIsNotReadableByAnybodyElse(t *testing.T) {
	store, dir := opened(t)
	config, token := aSession()

	if err := store.Save("books", config, token); err != nil {
		t.Fatalf("save: %v", err)
	}

	file, err := os.Stat(filepath.Join(dir, "books.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := file.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the session is mode %#o", mode)
	}

	// The directory too. Its listing says which services this deployment
	// holds credentials for, which is worth something to somebody who cannot
	// read the files themselves.
	held, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := held.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the directory is mode %#o", mode)
	}
}

func TestASessionOthersCanReadIsRefusedRatherThanUsed(t *testing.T) {
	store, dir := opened(t)
	config, token := aSession()

	if err := store.Save("books", config, token); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "books.json"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// Refused, not skipped. Reading it anyway would use a credential that
	// every account on this machine has had a chance to copy.
	_, err := store.Load("books")
	if err == nil {
		t.Fatal("a world-readable session was used")
	}
	if errors.Is(err, ErrNoSession) {
		t.Fatal("an exposed session was reported as no session at all")
	}
	if !strings.Contains(err.Error(), "600") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestReplacingASessionIsNeverHalfDone(t *testing.T) {
	store, dir := opened(t)
	config, token := aSession()

	if err := store.Save("books", config, token); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A reader running throughout a series of replacements. Every read must
	// see one whole session: a truncated file reads as signed out, which
	// sends somebody to a browser they did not need.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := store.Load("books")
			if err != nil {
				t.Errorf("a read during a replacement failed: %v", err)
				return
			}
			if got.Token.AccessToken == "" || got.Config == nil {
				t.Errorf("a read during a replacement saw half a session: %+v", got)
				return
			}
		}
	}()

	for round := range 40 {
		rotated := *token
		rotated.AccessToken = "access-" + string(rune('a'+round%26))
		rotated.RefreshToken = "refresh-" + string(rune('a'+round%26))
		if err := store.Save("books", config, &rotated); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	close(stop)
	readers.Wait()

	// And nothing left behind. A temp file that survived is a session at
	// whatever mode it was created with, sitting beside the real one.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "books.json" {
			t.Errorf("something was left behind: %s", entry.Name())
		}
	}
}

func TestOneServerIsOneFile(t *testing.T) {
	store, _ := opened(t)
	config, token := aSession()

	if err := store.Save("books", config, token); err != nil {
		t.Fatalf("save books: %v", err)
	}
	other := *token
	other.AccessToken = "access-elsewhere"
	if err := store.Save("weather", config, &other); err != nil {
		t.Fatalf("save weather: %v", err)
	}

	// Signing out of one is not signing out of the other.
	if err := store.Forget("books"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := store.Load("books"); !errors.Is(err, ErrNoSession) {
		t.Errorf("books survived being forgotten: %v", err)
	}
	kept, err := store.Load("weather")
	if err != nil {
		t.Fatalf("weather was lost with books: %v", err)
	}
	if kept.Token.AccessToken != "access-elsewhere" {
		t.Errorf("weather holds the wrong session: %+v", kept.Token)
	}
}

func TestASignOutThatNeverHappenedIsNotAnError(t *testing.T) {
	store, _ := opened(t)

	if err := store.Forget("never-used"); err != nil {
		t.Fatalf("forgetting nothing failed: %v", err)
	}
}

func TestAServerNameThatWouldEscapeTheDirectoryIsRefused(t *testing.T) {
	store, dir := opened(t)
	config, token := aSession()

	for _, name := range []string{"../escaped", "sub/dir", "..", ""} {
		if err := store.Save(name, config, token); err == nil {
			t.Errorf("a server named %q was stored", name)
		}
		if _, err := store.Load(name); err == nil {
			t.Errorf("a server named %q was loaded", name)
		}
	}

	// Nothing reached the parent, which is what the name was reaching for.
	above, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range above {
		if entry.Name() != DirName {
			t.Errorf("something was written outside the store: %s", entry.Name())
		}
	}
}
