package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// helperUnmarshalExchangeYAML is a test helper that runs UnmarshalYAML via yaml.Unmarshal
// on an arbitrary exchange config value given raw YAML bytes.
func mustDecodeExchangeYAML(t *testing.T, target yaml.Unmarshaler, data string) {
	t.Helper()
	if err := yaml.Unmarshal([]byte(data), target); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Binance
// ---------------------------------------------------------------------------

func TestBinanceExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	// Simulates: user submits new api_key + secret via PATCH → JSON unmarshal sets
	// them on newCfg → SecurityCopyFrom must NOT overwrite with old .security.yml values.
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q (old value overwrote the update)", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q (old value overwrote the update)", got)
	}
}

func TestBinanceExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	// Simulates first-time setup or field that the user left unchanged ([NOT_HERE]
	// round-trip leaves it empty → overlay must restore from .security.yml).
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{{Name: "main"}},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "STORED_SECRET" {
		t.Errorf("Secret: want STORED_SECRET, got %q", got)
	}
}

func TestBinanceExchangeConfig_UnmarshalYAML_PartialUpdate(t *testing.T) {
	// User changes only api_key; secret round-trips as [NOT_HERE] → left empty.
	// Overlay must keep the new api_key and fill secret from .security.yml.
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "main", APIKey: *NewSecureString("NEW_KEY")},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q (new value was overwritten)", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "STORED_SECRET" {
		t.Errorf("Secret: want STORED_SECRET (filled from security), got %q", got)
	}
}

func TestBinanceExchangeConfig_UnmarshalYAML_UnknownAccountKey(t *testing.T) {
	// Account name has no entry in .security.yml — fields remain as-is.
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "newaccount", APIKey: *NewSecureString("MY_KEY"), Secret: *NewSecureString("MY_SECRET")},
		},
	}

	secYAML := "oldaccount:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "MY_KEY" {
		t.Errorf("APIKey: want MY_KEY, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// BinanceTH
// ---------------------------------------------------------------------------

func TestBinanceTHExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	cfg := BinanceTHExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q", got)
	}
}

func TestBinanceTHExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	cfg := BinanceTHExchangeConfig{
		Accounts: []ExchangeAccount{{Name: "main"}},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Bitkub
// ---------------------------------------------------------------------------

func TestBitkubExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	cfg := BitkubExchangeConfig{
		Accounts: []ExchangeAccount{
			{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q", got)
	}
}

func TestBitkubExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	cfg := BitkubExchangeConfig{
		Accounts: []ExchangeAccount{{Name: "main"}},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// OKX (also has Passphrase)
// ---------------------------------------------------------------------------

func TestOKXExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	cfg := OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
				Passphrase:      *NewSecureString("NEW_PASS"),
			},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n  passphrase: OLD_PASS\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q", got)
	}
	if got := cfg.Accounts[0].Passphrase.String(); got != "NEW_PASS" {
		t.Errorf("Passphrase: want NEW_PASS, got %q", got)
	}
}

func TestOKXExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	cfg := OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n  passphrase: STORED_PASS\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Passphrase.String(); got != "STORED_PASS" {
		t.Errorf("Passphrase: want STORED_PASS, got %q", got)
	}
}

func TestOKXExchangeConfig_UnmarshalYAML_PartialPassphraseUpdate(t *testing.T) {
	// User changes only passphrase; api_key and secret are empty → filled from security.
	cfg := OKXExchangeConfig{
		Accounts: []OKXExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main"},
				Passphrase:      *NewSecureString("NEW_PASS"),
			},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n  passphrase: OLD_PASS\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Passphrase.String(); got != "NEW_PASS" {
		t.Errorf("Passphrase: want NEW_PASS (preserved), got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Settrade (also has PIN)
// ---------------------------------------------------------------------------

func TestSettradeExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	cfg := SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
				PIN:             *NewSecureString("NEW_PIN"),
			},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n  pin: OLD_PIN\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q", got)
	}
	if got := cfg.Accounts[0].PIN.String(); got != "NEW_PIN" {
		t.Errorf("PIN: want NEW_PIN, got %q", got)
	}
}

func TestSettradeExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	cfg := SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n  pin: STORED_PIN\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].PIN.String(); got != "STORED_PIN" {
		t.Errorf("PIN: want STORED_PIN, got %q", got)
	}
}

func TestSettradeExchangeConfig_UnmarshalYAML_PartialPINUpdate(t *testing.T) {
	// User changes only PIN; api_key and secret empty → filled from security.
	cfg := SettradeExchangeConfig{
		Accounts: []SettradeExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main"},
				PIN:             *NewSecureString("NEW_PIN"),
			},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n  pin: OLD_PIN\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].PIN.String(); got != "NEW_PIN" {
		t.Errorf("PIN: want NEW_PIN (preserved), got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Webull
// ---------------------------------------------------------------------------

func TestWebullExchangeConfig_UnmarshalYAML_PreservesNewCredential(t *testing.T) {
	cfg := WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main", APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
				AccountID:       "acct-1",
			},
		},
	}

	secYAML := "main:\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET, got %q", got)
	}
}

func TestWebullExchangeConfig_UnmarshalYAML_FillsEmptyCredential(t *testing.T) {
	cfg := WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{ExchangeAccount: ExchangeAccount{Name: "main"}},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "STORED_SECRET" {
		t.Errorf("Secret: want STORED_SECRET, got %q", got)
	}
}

func TestWebullExchangeConfig_UnmarshalYAML_PartialSecretUpdate(t *testing.T) {
	// User changes only Secret; api_key empty → filled from security.
	cfg := WebullExchangeConfig{
		Accounts: []WebullExchangeAccount{
			{
				ExchangeAccount: ExchangeAccount{Name: "main", Secret: *NewSecureString("NEW_SECRET")},
				AccountID:       "acct-1",
			},
		},
	}

	secYAML := "main:\n  api_key: STORED_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
	if got := cfg.Accounts[0].Secret.String(); got != "NEW_SECRET" {
		t.Errorf("Secret: want NEW_SECRET (preserved), got %q", got)
	}
}

// ---------------------------------------------------------------------------
// SecureModelList
// ---------------------------------------------------------------------------

func TestSecureModelList_UnmarshalYAML_PreservesNewAPIKey(t *testing.T) {
	list := SecureModelList{
		{ModelName: "gpt-4", Model: "openai/gpt-4o", APIKey: *NewSecureString("NEW_KEY")},
	}

	secYAML := "\"gpt-4:0\":\n  api_key: OLD_KEY\n"
	if err := yaml.Unmarshal([]byte(secYAML), &list); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if got := list[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q (old value overwrote the update)", got)
	}
}

func TestSecureModelList_UnmarshalYAML_FillsEmptyAPIKey(t *testing.T) {
	list := SecureModelList{
		{ModelName: "gpt-4", Model: "openai/gpt-4o"},
	}

	secYAML := "\"gpt-4:0\":\n  api_key: STORED_KEY\n"
	if err := yaml.Unmarshal([]byte(secYAML), &list); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	if got := list[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Positional-key accounts (no Name field)
// ---------------------------------------------------------------------------

func TestBinanceExchangeConfig_UnmarshalYAML_PositionalKey_PreservesNewCredential(t *testing.T) {
	// Accounts without a Name are keyed by ordinal "1", "2", etc.
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{
			{APIKey: *NewSecureString("NEW_KEY"), Secret: *NewSecureString("NEW_SECRET")},
		},
	}

	secYAML := "\"1\":\n  api_key: OLD_KEY\n  secret: OLD_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "NEW_KEY" {
		t.Errorf("APIKey: want NEW_KEY, got %q", got)
	}
}

func TestBinanceExchangeConfig_UnmarshalYAML_PositionalKey_FillsEmptyCredential(t *testing.T) {
	cfg := BinanceExchangeConfig{
		Accounts: []ExchangeAccount{{}},
	}

	secYAML := "\"1\":\n  api_key: STORED_KEY\n  secret: STORED_SECRET\n"
	mustDecodeExchangeYAML(t, &cfg, secYAML)

	if got := cfg.Accounts[0].APIKey.String(); got != "STORED_KEY" {
		t.Errorf("APIKey: want STORED_KEY, got %q", got)
	}
}
