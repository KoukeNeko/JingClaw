package sqlite

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// storedOrigin is a RunOrigin in a text column.
//
// The column is written and read back and never searched or ordered by, so
// what shape it holds is free. JSON, because the origin has parts and a
// flattened one would need a grammar, and a grammar in a column is a parser
// nobody remembers writing.
type storedOrigin struct{ domain.RunOrigin }

// Value writes the origin.
func (o storedOrigin) Value() (driver.Value, error) {
	if o.Kind == "" && o.ClientID == "" && o.Principal == nil {
		return "", nil
	}
	written, err := json.Marshal(o.RunOrigin)
	if err != nil {
		return nil, fmt.Errorf("sqlite: write origin: %w", err)
	}
	return string(written), nil
}

// Scan reads it, including from before the column held one.
//
// Rows written earlier hold a single string, and the only shape a running
// deployment ever produced was "<platform>:<principal id>" from a button
// press. Anything else named a client or a platform rather than a person, and
// is kept where it is: turning one of those into a principal would be the
// program claiming to know who authorised something when it does not.
func (o *storedOrigin) Scan(value any) error {
	switch held := value.(type) {
	case nil:
		o.RunOrigin = domain.RunOrigin{}
		return nil
	case []byte:
		return o.read(string(held))
	case string:
		return o.read(held)
	}
	return fmt.Errorf("sqlite: read origin: unexpected %T", value)
}

func (o *storedOrigin) read(held string) error {
	held = strings.TrimSpace(held)
	if held == "" {
		o.RunOrigin = domain.RunOrigin{}
		return nil
	}

	if strings.HasPrefix(held, "{") {
		if err := json.Unmarshal([]byte(held), &o.RunOrigin); err != nil {
			return fmt.Errorf("sqlite: read origin: %w", err)
		}
		return nil
	}

	platform, id, split := strings.Cut(held, ":")
	if !split || platform == "" || id == "" {
		o.RunOrigin = domain.RunOrigin{Kind: domain.OriginGateway, ClientID: held}
		return nil
	}
	o.RunOrigin = domain.RunOrigin{
		Kind:      domain.OriginGateway,
		Principal: &domain.ExternalPrincipal{Platform: platform, PrincipalID: id},
	}
	return nil
}
