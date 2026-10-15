package chatgptauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// record is the whole file.
//
// The host id and the issued client id outlive a sign-out: the documentation
// says to keep the account-to-client mapping and the host id so that signing
// in again reuses the registration. The tokens do not.
type record struct {
	Version int `json:"version"`

	// HostID is a UUIDv4 with the urn:uuid: prefix, generated once. It is not
	// a credential, and says nothing about who the person is.
	HostID string `json:"host_id"`

	// ClientID is the oaiapp_... id the first authorization was issued.
	ClientID string `json:"client_id,omitempty"`

	// Who the validated ID token said. The subject is the identity; the email
	// is only a label and a hint, and may be shared by other registrations.
	Subject string `json:"subject,omitempty"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`

	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`

	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
}

func (r record) signedIn() bool { return r.AccessToken != "" || r.RefreshToken != "" }

func (r record) planUsage() bool {
	for _, scope := range r.Scopes {
		if scope == ScopePlanUsage {
			return true
		}
	}
	return false
}

// DefaultPath is where this deployment's sign-in is kept: beside the other
// things the deployment owns, and the same for the daemon that signs in and the
// one that reads it.
func DefaultPath() string {
	dir, found := home.Resolve()
	if !found {
		return filepath.Join(os.TempDir(), "jingclaw-"+FileName)
	}
	return filepath.Join(dir.Root, FileName)
}

// load reads the file. A file that is not there is an empty record.
//
// A file other accounts can read is refused rather than used: what is in it is
// somebody's ChatGPT plan.
func load(path string) (record, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return record{Version: 1}, nil
		}
		return record{}, fmt.Errorf("chatgpt: %w", err)
	}

	ownerOnly, exposure, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		return record{}, fmt.Errorf("chatgpt: check %s: %w", path, err)
	}
	if !ownerOnly {
		return record{}, fmt.Errorf("chatgpt: %s %s; it must be readable only by its owner (chmod 600)", path, exposure)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return record{}, fmt.Errorf("chatgpt: %w", err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		// The parse error is not quoted: it can carry a fragment of the file.
		return record{}, fmt.Errorf("chatgpt: %s is not a sign-in this can read", path)
	}
	return rec, nil
}

// save replaces the file whole: written to a temp file beside it, restricted
// before any byte goes in, flushed, and renamed. A reader sees the old record
// or the new one, never half of either - and a rotated refresh token is on disk
// before anything uses it.
func save(path string, rec record) error {
	rec.Version = 1
	encoded, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".chatgpt-auth-*")
	if err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	defer os.Remove(temp.Name())

	if err := fsperm.Restrict(temp.Name()); err != nil {
		temp.Close()
		return fmt.Errorf("chatgpt: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("chatgpt: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("chatgpt: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("chatgpt: %w", err)
	}
	return nil
}

// newHostID is a UUIDv4 in the urn:uuid: form the documentation accepts.
func newHostID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("chatgpt: could not generate a host id")
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return "urn:uuid:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
