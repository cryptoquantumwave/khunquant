package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// The placeholder accounts of an instance (sandbox.ProfileOptions.Accounts) are written
// by the profile alone. A config patch must not touch the accounts of such a venue: a
// merge patch replaces an array whole, so an `accounts: [{name: main}]` keeps the
// placeholder credentials (they pair with the account by name) but drops its permissions
// list, and an empty list means every scope, transfers included.

// accountsFile records the venues of an instance that have a placeholder account, so a
// patch applied later (by another process) is checked against them too.
const accountsFile = ".sandbox-accounts"

// CheckAccounts refuses a venue that has no simulator, and a venue named twice.
func CheckAccounts(venues []string) error {
	for i, v := range venues {
		switch {
		case !slices.Contains(sandbox.AccountVenues, v):
			return fmt.Errorf("sandbox: accounts: %q has no simulator (only %v do)", v, sandbox.AccountVenues)
		case slices.Contains(venues[:i], v):
			return fmt.Errorf("sandbox: accounts: %q is named twice", v)
		}
	}
	return nil
}

func writeAccounts(home string, venues []string) error {
	if len(venues) == 0 {
		return nil
	}
	b, _ := json.Marshal(venues)
	return os.WriteFile(filepath.Join(home, accountsFile), b, 0o600)
}

// readAccounts lists the venues recorded for the home; none if there is no record.
func readAccounts(home string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(home, accountsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var venues []string
	return venues, json.Unmarshal(b, &venues)
}

// CheckAccountPatch refuses a patch that could replace the accounts of a venue in venues:
// its accounts key, or the object it is in being replaced by null or a scalar.
func CheckAccountPatch(patch map[string]any, venues []string) error {
	if len(venues) == 0 {
		return nil
	}
	for k, v := range patch {
		if fold(k) != fold("exchanges") {
			continue
		}
		ex, ok := v.(map[string]any)
		if !ok {
			return errors.New("sandbox: config patch refused at exchanges: it must be an object while the instance has placeholder accounts")
		}
		for vk, vv := range ex {
			if !slices.ContainsFunc(venues, func(name string) bool { return fold(vk) == fold(name) }) {
				continue
			}
			obj, ok := vv.(map[string]any)
			if !ok {
				return fmt.Errorf("sandbox: config patch refused at exchanges.%s: it must be an object while the instance has a placeholder account", vk)
			}
			for ak := range obj {
				if fold(ak) == fold("accounts") {
					return fmt.Errorf("sandbox: config patch refused at exchanges.%s.accounts: the sandbox profile owns this venue's placeholder account", vk)
				}
			}
		}
	}
	return nil
}
