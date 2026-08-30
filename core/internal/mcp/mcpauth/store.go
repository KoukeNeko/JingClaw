package mcpauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// DirName is where sessions live, under the deployment directory.
const DirName = "mcp-auth"

// Session is everything needed to keep talking to one server.
//
// The whole of it, not just the refresh token. An authorization server may
// rotate the refresh token on every use and may have issued the client
// credentials itself through dynamic registration, so a store holding one
// string is one that works until the first refresh.
type Session struct {
	// Config is the oauth2 endpoints and client this was obtained with.
	Config *oauth2.Config `json:"config"`

	// Token is the current access token, and the refresh token if there is
	// one. There may not be: a client cannot assume one was issued.
	Token *oauth2.Token `json:"token"`
}

// Store keeps each server's session in a file of its own.
//
// One file per server rather than one file for all of them. Signing in to one
// server and refreshing another are separate processes writing at the same
// time, and a shared file makes that a lock; separate files make it nothing.
// It also bounds the damage: a file somebody corrupted loses one server.
type Store struct {
	dir string

	// mu serialises refresh within this process.
	//
	// It matters because refresh tokens rotate. Two goroutines refreshing the
	// same session concurrently will have one of them present a token the
	// server has already retired, get invalid_grant back, and conclude that
	// perfectly good credentials are dead.
	//
	// Between processes there is no lock, only the atomic rename in save: a
	// reader sees the old session or the new one, never half of either. That
	// is enough because the two writers are a daemon refreshing and a person
	// signing in, and a person signs in when the daemon has already given up.
	mu sync.Mutex
}

// Open prepares the directory sessions are kept in.
func Open(dir string) (*Store, error) {
	// 0700, because what is in here is equivalent to a password. The file
	// modes below say the same thing again; a directory somebody else can
	// list is one where they can see which services this deployment holds
	// credentials for even if they cannot read them.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mcpauth: could not make %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// ErrNoSession says nobody has signed in to this server.
var ErrNoSession = errors.New("mcpauth: no stored session")

// Load reads a server's session.
func (s *Store) Load(server string) (*Session, error) {
	path, err := s.pathFor(server)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoSession
		}
		return nil, fmt.Errorf("mcpauth: %w", err)
	}

	// A session readable by other local accounts is one to treat as
	// compromised. Refused rather than skipped, the same way this deployment
	// already treats a credential file: falling through quietly would hide
	// the exposure, and what is behind it is somebody's account.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf(
			"mcpauth: %s is mode %#o; it must not be readable by group or others (chmod 600)",
			path, mode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcpauth: %w", err)
	}

	var session Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, fmt.Errorf("mcpauth: %s is not a session this can read: %w", path, err)
	}
	if session.Token == nil {
		return nil, fmt.Errorf("mcpauth: %s holds no token", path)
	}
	return &session, nil
}

// Save writes a server's session, replacing whatever was there.
//
// Written whole and renamed into place. A process reading this file while it
// is being replaced gets the old session or the new one; the alternative is a
// truncated file, which reads as "signed out" and sends somebody to a browser
// they did not need to visit.
func (s *Store) Save(server string, config *oauth2.Config, token *oauth2.Token) error {
	if token == nil {
		return errors.New("mcpauth: refusing to store a session with no token")
	}

	path, err := s.pathFor(server)
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(Session{Config: config, Token: token}, "", "  ")
	if err != nil {
		return fmt.Errorf("mcpauth: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// In the same directory, so the rename is within one filesystem and is
	// therefore atomic. A temp file in /tmp would make this a copy.
	temp, err := os.CreateTemp(s.dir, ".session-*")
	if err != nil {
		return fmt.Errorf("mcpauth: %w", err)
	}
	defer os.Remove(temp.Name())

	// Before the bytes, not after: a file that exists at 0600 from the moment
	// it has content is one nobody can read in between.
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("mcpauth: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("mcpauth: %w", err)
	}
	// Flushed before the rename, so a machine that loses power between the
	// two finds the old session rather than an empty new one.
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("mcpauth: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("mcpauth: %w", err)
	}

	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("mcpauth: %w", err)
	}
	return nil
}

// Forget discards a server's session, for signing out.
//
// Only ever called because somebody asked. A refresh that failed does not
// come here: an authorization server having a bad hour would otherwise turn
// recoverable credentials into a browser somebody has to go and find.
func (s *Store) Forget(server string) error {
	path, err := s.pathFor(server)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mcpauth: %w", err)
	}
	return nil
}

// pathFor is where one server's session lives.
//
// The name is checked rather than escaped. A server called "../../id_rsa" is
// a configuration somebody has to have written by hand, and the useful
// response to it is to say so — an escaped version would silently store the
// session under a name that is not the one in the settings.
func (s *Store) pathFor(server string) (string, error) {
	if server == "" {
		return "", errors.New("mcpauth: a session needs a server name")
	}
	if strings.ContainsAny(server, `/\`) || server == "." || server == ".." {
		return "", fmt.Errorf("mcpauth: %q cannot name a file", server)
	}
	return filepath.Join(s.dir, server+".json"), nil
}

// DefaultDir is where this deployment's sign-ins live.
//
// One function rather than one per caller. The daemon reads what the CLI
// wrote, so the two agreeing is not a convention to be kept but the whole
// mechanism: a second copy of this that drifted would be a sign-in that
// appeared to succeed and a daemon that never saw it.
func DefaultDir() string {
	dir, found := home.Resolve()
	if !found {
		// No deployment, so nothing durable to belong to. Still a real path,
		// because the alternative is a command that fails for a reason nobody
		// asked about.
		return filepath.Join(os.TempDir(), "jingclaw-"+DirName)
	}
	return filepath.Join(dir.Root, DirName)
}
