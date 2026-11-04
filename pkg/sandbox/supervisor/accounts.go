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

// The placeholder accounts of an instance are put in by the locked gateway
// (sandbox.ApplyPlaceholderAccounts, on every load and reload) for the venues recorded in
// sandbox.AccountsFile, so a config patch cannot change them: whatever it says about those
// accounts is replaced. CheckAccountPatch refuses such a patch early and clearly instead of
// letting it be silently overridden. (A merge patch replaces an array whole, so without the
// overlay an `accounts: [{name: main}]` would drop the permissions list, and an empty list
// means every scope, transfers included.)

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
	return os.WriteFile(filepath.Join(home, sandbox.AccountsFile), b, 0o600)
}

// readAccounts lists the venues recorded for the home; none if there is no record.
func readAccounts(home string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(home, sandbox.AccountsFile))
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
