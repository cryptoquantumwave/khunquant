package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	khunquantconfig "github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges/webull"
)

// --------------------------------------------------------------------------
// Exchange menu
// --------------------------------------------------------------------------

func (s *appState) buildExchangeMenuItems() []MenuItem {
	ex := s.config.Exchanges
	return []MenuItem{
		exchangeItem("Binance", "Spot / Futures trading", ex.Binance.Enabled, func() {
			s.push("exchange-binance", s.binanceMenu())
		}),
		exchangeItem("Binance TH", "Binance Thailand", ex.BinanceTH.Enabled, func() {
			s.push("exchange-binanceth", s.binancethMenu())
		}),
		exchangeItem("Bitkub", "Thai crypto exchange", ex.Bitkub.Enabled, func() {
			s.push("exchange-bitkub", s.bitkubMenu())
		}),
		exchangeItem("OKX", "OKX global exchange", ex.OKX.Enabled, func() {
			s.push("exchange-okx", s.okxMenu())
		}),
		exchangeItem("Settrade", "Thai stock broker (SET)", ex.Settrade.Enabled, func() {
			s.push("exchange-settrade", s.settradeMenu())
		}),
		exchangeItem("Webull", "US stock broker", ex.Webull.Enabled, func() {
			s.push("exchange-webull", s.webullMenu())
		}),
	}
}

func (s *appState) exchangeMenu() tview.Primitive {
	menu := NewMenu("Exchanges", s.buildExchangeMenuItems())
	menu.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			s.pop()
			return nil
		}
		return event
	})
	return menu
}

func refreshExchangeMenuFromState(menu *Menu, s *appState) {
	menu.applyItems(s.buildExchangeMenuItems())
}

func exchangeItem(label, description string, enabled bool, action MenuAction) MenuItem {
	item := MenuItem{
		Label:       label,
		Description: description,
		Action:      action,
	}
	if !enabled {
		color := tcell.ColorGray
		item.MainColor = &color
	}
	return item
}

// --------------------------------------------------------------------------
// Per-exchange account list menus (list accounts + "Add account")
// --------------------------------------------------------------------------

func (s *appState) binanceMenu() tview.Primitive {
	return s.genericAccountMenu("Binance", "exchange-binance",
		func() int { return len(s.config.Exchanges.Binance.Accounts) },
		func(i int) string { return s.config.Exchanges.Binance.Accounts[i].Name },
		func(i int) tview.Primitive { return s.binanceAccountForm(i) },
		func() {
			s.config.Exchanges.Binance.Accounts = append(
				s.config.Exchanges.Binance.Accounts,
				khunquantconfig.ExchangeAccount{Name: s.nextAccountName(accountNames(s.config.Exchanges.Binance.Accounts))},
			)
			s.config.Exchanges.Binance.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-binance", s.binanceMenu())
		},
	)
}

func (s *appState) binancethMenu() tview.Primitive {
	return s.genericAccountMenu("Binance TH", "exchange-binanceth",
		func() int { return len(s.config.Exchanges.BinanceTH.Accounts) },
		func(i int) string { return s.config.Exchanges.BinanceTH.Accounts[i].Name },
		func(i int) tview.Primitive { return s.binancethAccountForm(i) },
		func() {
			s.config.Exchanges.BinanceTH.Accounts = append(
				s.config.Exchanges.BinanceTH.Accounts,
				khunquantconfig.ExchangeAccount{Name: s.nextAccountName(accountNames(s.config.Exchanges.BinanceTH.Accounts))},
			)
			s.config.Exchanges.BinanceTH.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-binanceth", s.binancethMenu())
		},
	)
}

func (s *appState) bitkubMenu() tview.Primitive {
	return s.genericAccountMenu("Bitkub", "exchange-bitkub",
		func() int { return len(s.config.Exchanges.Bitkub.Accounts) },
		func(i int) string { return s.config.Exchanges.Bitkub.Accounts[i].Name },
		func(i int) tview.Primitive { return s.bitkubAccountForm(i) },
		func() {
			s.config.Exchanges.Bitkub.Accounts = append(
				s.config.Exchanges.Bitkub.Accounts,
				khunquantconfig.ExchangeAccount{Name: s.nextAccountName(accountNames(s.config.Exchanges.Bitkub.Accounts))},
			)
			s.config.Exchanges.Bitkub.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-bitkub", s.bitkubMenu())
		},
	)
}

func (s *appState) okxMenu() tview.Primitive {
	return s.genericAccountMenu("OKX", "exchange-okx",
		func() int { return len(s.config.Exchanges.OKX.Accounts) },
		func(i int) string { return s.config.Exchanges.OKX.Accounts[i].Name },
		func(i int) tview.Primitive { return s.okxAccountForm(i) },
		func() {
			s.config.Exchanges.OKX.Accounts = append(
				s.config.Exchanges.OKX.Accounts,
				khunquantconfig.OKXExchangeAccount{
					ExchangeAccount: khunquantconfig.ExchangeAccount{
						Name: s.nextAccountName(accountNames(s.config.Exchanges.OKX.Accounts)),
					},
				},
			)
			s.config.Exchanges.OKX.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-okx", s.okxMenu())
		},
	)
}

func (s *appState) settradeMenu() tview.Primitive {
	return s.genericAccountMenu("Settrade", "exchange-settrade",
		func() int { return len(s.config.Exchanges.Settrade.Accounts) },
		func(i int) string { return s.config.Exchanges.Settrade.Accounts[i].Name },
		func(i int) tview.Primitive { return s.settradeAccountForm(i) },
		func() {
			s.config.Exchanges.Settrade.Accounts = append(
				s.config.Exchanges.Settrade.Accounts,
				khunquantconfig.SettradeExchangeAccount{
					ExchangeAccount: khunquantconfig.ExchangeAccount{
						Name: s.nextAccountName(accountNames(s.config.Exchanges.Settrade.Accounts)),
					},
				},
			)
			s.config.Exchanges.Settrade.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-settrade", s.settradeMenu())
		},
	)
}

func (s *appState) webullMenu() tview.Primitive {
	return s.genericAccountMenu("Webull", "exchange-webull",
		func() int { return len(s.config.Exchanges.Webull.Accounts) },
		func(i int) string { return s.config.Exchanges.Webull.Accounts[i].Name },
		func(i int) tview.Primitive { return s.webullAccountForm(i) },
		func() {
			s.config.Exchanges.Webull.Accounts = append(
				s.config.Exchanges.Webull.Accounts,
				khunquantconfig.WebullExchangeAccount{
					ExchangeAccount: khunquantconfig.ExchangeAccount{
						Name: s.nextAccountName(accountNames(s.config.Exchanges.Webull.Accounts)),
					},
					// Thailand is the only supported Webull region today;
					// see pkg/exchanges/webull.NormalizeRegion.
					Region: webull.DefaultRegion,
				},
			)
			s.config.Exchanges.Webull.Enabled = true
			s.dirty = true
			s.pop()
			s.push("exchange-webull", s.webullMenu())
		},
	)
}

// genericAccountMenu builds a menu listing existing accounts plus "Add account".
func (s *appState) genericAccountMenu(
	title, pageKey string,
	countFn func() int,
	nameFn func(int) string,
	formFn func(int) tview.Primitive,
	addFn func(),
) tview.Primitive {
	n := countFn()
	items := make([]MenuItem, 0, n+1)
	for i := 0; i < n; i++ {
		idx := i
		name := nameFn(i)
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		}
		items = append(items, MenuItem{
			Label:       name,
			Description: "Edit account credentials",
			Action:      func() { s.push(fmt.Sprintf("%s-%d", pageKey, idx), formFn(idx)) },
		})
	}
	items = append(items, MenuItem{
		Label:       "**Add account**",
		Description: "Append a new account",
		Action:      addFn,
	})

	menu := NewMenu(title+" Accounts", items)
	menu.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			s.pop()
			return nil
		}
		return event
	})
	return menu
}

// --------------------------------------------------------------------------
// Per-exchange account forms
// --------------------------------------------------------------------------

func (s *appState) binanceAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.Binance.Accounts[index]
	form := baseExchangeAccountForm("Binance", acc.Name)
	addInput(form, "API Key", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
		if menu, ok := s.menus["exchange"]; ok {
			refreshExchangeMenuFromState(menu, s)
		}
	})
	addInput(form, "Secret", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.Binance.Accounts = removeAccount(s.config.Exchanges.Binance.Accounts, index)
		if len(s.config.Exchanges.Binance.Accounts) == 0 {
			s.config.Exchanges.Binance.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

func (s *appState) binancethAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.BinanceTH.Accounts[index]
	form := baseExchangeAccountForm("Binance TH", acc.Name)
	addInput(form, "API Key", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
	})
	addInput(form, "Secret", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.BinanceTH.Accounts = removeAccount(s.config.Exchanges.BinanceTH.Accounts, index)
		if len(s.config.Exchanges.BinanceTH.Accounts) == 0 {
			s.config.Exchanges.BinanceTH.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

func (s *appState) bitkubAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.Bitkub.Accounts[index]
	form := baseExchangeAccountForm("Bitkub", acc.Name)
	addInput(form, "API Key", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
	})
	addInput(form, "Secret", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.Bitkub.Accounts = removeAccount(s.config.Exchanges.Bitkub.Accounts, index)
		if len(s.config.Exchanges.Bitkub.Accounts) == 0 {
			s.config.Exchanges.Bitkub.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

func (s *appState) okxAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.OKX.Accounts[index]
	form := baseExchangeAccountForm("OKX", acc.Name)
	addInput(form, "API Key", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
	})
	addInput(form, "Secret", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addInput(form, "Passphrase", acc.Passphrase.String(), func(v string) {
		acc.Passphrase.Set(v)
		s.dirty = true
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.OKX.Accounts = removeAccount(s.config.Exchanges.OKX.Accounts, index)
		if len(s.config.Exchanges.OKX.Accounts) == 0 {
			s.config.Exchanges.OKX.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

func (s *appState) settradeAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.Settrade.Accounts[index]
	form := baseExchangeAccountForm("Settrade", acc.Name)
	addInput(form, "API Key (login ID)", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
	})
	addInput(form, "Secret (PKCS#8 private key, base64)", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addInput(form, "Broker ID (e.g. FSSVP)", acc.BrokerID, func(v string) {
		acc.BrokerID = v
		s.dirty = true
	})
	addInput(form, "App Code (e.g. ALGO)", acc.AppCode, func(v string) {
		acc.AppCode = v
		s.dirty = true
	})
	addInput(form, "Account No", acc.AccountNo, func(v string) {
		acc.AccountNo = v
		s.dirty = true
	})
	addInput(form, "PIN (optional)", acc.PIN.String(), func(v string) {
		acc.PIN.Set(v)
		s.dirty = true
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.Settrade.Accounts = removeAccount(s.config.Exchanges.Settrade.Accounts, index)
		if len(s.config.Exchanges.Settrade.Accounts) == 0 {
			s.config.Exchanges.Settrade.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

func (s *appState) webullAccountForm(index int) tview.Primitive {
	acc := &s.config.Exchanges.Webull.Accounts[index]
	form := baseExchangeAccountForm("Webull", acc.Name)
	addInput(form, "API Key (app key)", acc.APIKey.String(), func(v string) {
		acc.APIKey.Set(v)
		s.dirty = true
		refreshMainMenuIfPresent(s)
	})
	addInput(form, "Secret (app secret)", acc.Secret.String(), func(v string) {
		acc.Secret.Set(v)
		s.dirty = true
	})
	addInput(form, "Account ID", acc.AccountID, func(v string) {
		acc.AccountID = v
		s.dirty = true
	})
	// Region is fixed: Thailand is the only Webull regional broker this
	// integration is verified against, and an editable field here is how a
	// mismatched region (which fails with an opaque 401) gets configured by
	// accident. Shown read-only rather than hidden so the value is visible.
	form.AddTextView("Region", webull.DefaultRegion+" (Thailand — only region supported)", 48, 1, true, false)
	form.AddButton("Resolve Account ID", func() {
		s.resolveWebullAccountID(index)
	})
	addExchangeDeleteButton(form, s, func() {
		s.config.Exchanges.Webull.Accounts = removeAccount(s.config.Exchanges.Webull.Accounts, index)
		if len(s.config.Exchanges.Webull.Accounts) == 0 {
			s.config.Exchanges.Webull.Enabled = false
		}
	})
	return wrapWithBack(form, s)
}

// resolveWebullAccountID looks up the Webull brokerage account(s) that the
// entered app key/secret grant access to (via GET /openapi/account/list,
// which — like the official Webull SDK — only needs app credentials, not an
// account_id) and fills in Account ID. A single account is applied
// automatically; multiple accounts are presented for the user to pick from
// rather than guessed, since silently choosing the wrong one would read or
// trade against the wrong brokerage account.
func (s *appState) resolveWebullAccountID(index int) {
	acc := s.config.Exchanges.Webull.Accounts[index]
	if acc.APIKey.String() == "" || acc.Secret.String() == "" {
		s.showWebullResolveModal("Enter API Key and Secret first, then Resolve Account ID.")
		return
	}

	pageName := "webull-resolving"
	modal := tview.NewModal().SetText("Resolving Webull account...")
	modal.SetTitle("Please wait").SetBorder(true)
	s.pages.AddPage(pageName, modal, true, true)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// WithSessionPersistence: reuse an already-approved on-disk session
		// (the account list needs an access token), and persist any login
		// this starts so an in-app approval isn't wasted.
		client, err := webull.NewClient(acc, webull.WithSessionPersistence())
		var accounts []webull.AccountListItem
		if err == nil {
			accounts, err = client.FetchAccountList(ctx)
		}

		s.app.QueueUpdateDraw(func() {
			s.pages.RemovePage(pageName)
			s.handleWebullAccountListResult(index, accounts, err)
		})
	}()
}

// handleWebullAccountListResult applies the outcome of resolveWebullAccountID
// on the UI thread: single match applies automatically, no match or an
// error is reported, and multiple matches are offered as a pick-one modal.
func (s *appState) handleWebullAccountListResult(index int, accounts []webull.AccountListItem, err error) {
	if err != nil {
		s.showWebullResolveModal(fmt.Sprintf("Could not resolve account_id: %v", err))
		return
	}
	switch len(accounts) {
	case 0:
		s.showWebullResolveModal("No brokerage accounts were found for these app credentials. Verify API access has been approved for this app.")
	case 1:
		s.applyWebullAccountID(index, accounts[0].AccountID)
		s.showWebullResolveModal(fmt.Sprintf("Resolved account_id: %s", accounts[0].AccountID))
	default:
		s.showWebullAccountPicker(index, accounts)
	}
}

// applyWebullAccountID stores the resolved account_id and refreshes the form
// so the "Account ID" field reflects the new value — same pop-then-push
// refresh idiom used elsewhere in this file (e.g. webullMenu's "Add account").
func (s *appState) applyWebullAccountID(index int, accountID string) {
	s.config.Exchanges.Webull.Accounts[index].AccountID = accountID
	s.dirty = true
	pageKey := fmt.Sprintf("exchange-webull-%d", index)
	s.pop()
	s.push(pageKey, s.webullAccountForm(index))
}

// showWebullAccountPicker lets the user pick which brokerage account to use
// when the app credentials map to more than one (e.g. cash + margin + IRA).
func (s *appState) showWebullAccountPicker(index int, accounts []webull.AccountListItem) {
	pageName := "webull-account-picker"
	labels := make([]string, 0, len(accounts)+1)
	for _, a := range accounts {
		label := a.AccountLabel
		if label == "" {
			label = a.AccountType
		}
		labels = append(labels, fmt.Sprintf("%s (%s)", label, a.AccountID))
	}
	labels = append(labels, "Cancel")

	modal := tview.NewModal().
		SetText("Multiple brokerage accounts were found. Pick one:").
		AddButtons(labels).
		SetDoneFunc(func(buttonIndex int, _ string) {
			s.pages.RemovePage(pageName)
			if buttonIndex >= 0 && buttonIndex < len(accounts) {
				s.applyWebullAccountID(index, accounts[buttonIndex].AccountID)
			}
		})
	modal.SetTitle("Select Webull Account").SetBorder(true)
	s.pages.AddPage(pageName, modal, true, true)
}

// showWebullResolveModal shows a dismissable status/error message.
func (s *appState) showWebullResolveModal(text string) {
	pageName := "webull-resolve-status"
	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(_ int, _ string) {
			s.pages.RemovePage(pageName)
		})
	modal.SetTitle("Webull").SetBorder(true)
	s.pages.AddPage(pageName, modal, true, true)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func baseExchangeAccountForm(exchange, accountName string) *tview.Form {
	title := fmt.Sprintf("Exchange: %s / %s", exchange, accountName)
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(title)
	form.SetButtonBackgroundColor(tcell.NewRGBColor(80, 250, 123))
	form.SetButtonTextColor(tcell.NewRGBColor(12, 13, 22))
	return form
}

func addExchangeDeleteButton(form *tview.Form, s *appState, doDelete func()) {
	form.AddButton("Delete", func() {
		pageName := "confirm-delete-exchange-account"
		if s.pages.HasPage(pageName) {
			return
		}
		modal := tview.NewModal().
			SetText("Delete this account?").
			AddButtons([]string{"Cancel", "Delete"}).
			SetDoneFunc(func(_ int, buttonLabel string) {
				s.pages.RemovePage(pageName)
				if buttonLabel == "Delete" {
					doDelete()
					s.dirty = true
					refreshMainMenuIfPresent(s)
					if menu, ok := s.menus["exchange"]; ok {
						refreshExchangeMenuFromState(menu, s)
					}
					s.pop()
					s.pop() // back to exchange menu
				}
			})
		modal.SetTitle("Confirm Delete").SetBorder(true)
		s.pages.AddPage(pageName, modal, true, true)
	})
}

// removeAccount returns accounts with the element at index removed, for any
// account slice type. A no-op for an out-of-range index.
func removeAccount[T any](accounts []T, index int) []T {
	if index < 0 || index >= len(accounts) {
		return accounts
	}
	return append(accounts[:index:index], accounts[index+1:]...)
}

// accountNames returns the Name of each account, for any account type that
// exposes GetName() (every config account embeds config.ExchangeAccount).
func accountNames[T interface{ GetName() string }](accounts []T) []string {
	names := make([]string, len(accounts))
	for i, a := range accounts {
		names[i] = a.GetName()
	}
	return names
}

func (s *appState) nextAccountName(existing []string) string {
	base := "main"
	for _, n := range existing {
		if strings.EqualFold(strings.TrimSpace(n), base) {
			goto numbered
		}
	}
	return base
numbered:
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("account-%d", i)
		found := false
		for _, n := range existing {
			if strings.EqualFold(strings.TrimSpace(n), candidate) {
				found = true
				break
			}
		}
		if !found {
			return candidate
		}
	}
}

func (s *appState) hasEnabledExchange() bool {
	ex := s.config.Exchanges
	return ex.Binance.Enabled || ex.BinanceTH.Enabled || ex.Bitkub.Enabled ||
		ex.OKX.Enabled || ex.Settrade.Enabled || ex.Webull.Enabled
}

func (s *appState) countExchanges() (enabled, total int) {
	ex := s.config.Exchanges
	entries := []bool{
		ex.Binance.Enabled,
		ex.BinanceTH.Enabled,
		ex.Bitkub.Enabled,
		ex.OKX.Enabled,
		ex.Settrade.Enabled,
		ex.Webull.Enabled,
	}
	total = len(entries)
	for _, v := range entries {
		if v {
			enabled++
		}
	}
	return enabled, total
}

func rootExchangeLabel(enabledCount, total int) string {
	if enabledCount == 0 {
		return "Exchange (none enabled)"
	}
	return fmt.Sprintf("Exchange (%d/%d)", enabledCount, total)
}
