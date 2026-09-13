package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A subscription-mode codex reads $CODEX_HOME/auth.json before it reaches the
// network and refuses to start without one. In native mode the credential it
// would hold is not the container's to have: the proxy terminates the vendor
// connection and builds the upstream header from the resolved subscription. So
// the file exists, shaped the way codex expects, and holds nothing.
//
// The id_token is a syntactically valid RS256 JWT whose claim set is empty --
// enough to decode, carrying no identity.
const codexPlaceholderIDToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.e30." +
	"upI8kdqCUNUUgd1IrUpjNDDiif7yJZT_pI03g_DW6-aFIxEZD_kszt6E33_cjiUv6tWkutqTDgLr8XfKzFVfBKUTA9QDhpY9" +
	"Imavnu-CW5k6xSUdiSiwo5b7EyMGBO7bRPN9b0L3OL2CzqowqOalYiqY0lldy1IDUgD_n5Cm0CFLpMOipb_vGf2KJFYmR8T_" +
	"oZOAJzf6FYbZKFhjujeXiVLah2kj2qIZMIws9Q5t485udznl_gNlQwcnVB3bqEd6_msgUOo0ZRkyctQz9rZ70-JBviwXzhiq" +
	"oeDGeiqJeRbaWLOjhmpWlwc6DJgRgP1H59dzV9htOWjST6cs8vpG2A"

type codexAuth struct {
	AuthMode     string          `json:"auth_mode"`
	OpenAIAPIKey *string         `json:"OPENAI_API_KEY"`
	Tokens       codexAuthTokens `json:"tokens"`
	LastRefresh  string          `json:"last_refresh"`
}

type codexAuthTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// writeCodexAuth writes the file in native mode, in holder mode too: a sandbox
// spawns no CLI, but the engineer who runs codex by hand meets the same refusal.
//
// An existing file is left alone. It may be a real credential from the CLI's own
// login, and replacing that with a blank one logs the engineer out.
func writeCodexAuth(now time.Time) error {
	home, err := codexStateHome()
	if err != nil {
		return err
	}
	path := filepath.Join(home, "auth.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	payload, err := json.Marshal(codexAuth{
		AuthMode: "chatgpt",
		Tokens:   codexAuthTokens{IDToken: codexPlaceholderIDToken},
		// Stamped at start, not fixed: codex refreshes a credential it reads as
		// stale, and that refresh goes to an auth host the platform does not
		// intercept, so it fails and takes the CLI with it.
		LastRefresh: now.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("marshal codex auth: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
