package webull

import (
	"context"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// WebullExchange wraps webullAdapter and implements exchanges.PricedExchange
// for compatibility with portfolio tools.
type WebullExchange struct {
	adapter *webullAdapter
}

func (e *WebullExchange) Name() string { return Name }

func (e *WebullExchange) SupportedWalletTypes() []string {
	return e.adapter.SupportedWalletTypes()
}

func (e *WebullExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	bs, err := e.adapter.GetBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.Balance, len(bs))
	for i, b := range bs {
		out[i] = exchanges.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked}
	}
	return out, nil
}

func (e *WebullExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	bs, err := e.adapter.GetWalletBalances(ctx, walletType)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.WalletBalance, len(bs))
	for i, b := range bs {
		out[i] = exchanges.WalletBalance{
			Balance:    exchanges.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked},
			WalletType: b.WalletType,
			Extra:      b.Extra,
		}
	}
	return out, nil
}

func (e *WebullExchange) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return e.adapter.FetchPrice(ctx, asset, quote)
}

// SupportedQuotes implements exchanges.QuoteLister.
func (e *WebullExchange) SupportedQuotes() []string {
	return []string{"USD"}
}

// Reconnect implements exchanges.ReauthExchange by forcing a fresh
// token/create request. See Client.getOrRefreshToken / CreateToken for why
// this is a fast, synchronous "start a login" step distinct from the
// polling done in CheckReauth.
//
// It short-circuits if a session is already NORMAL and not close to
// expiring: Reconnect is exposed as a UI "Connect" button the user can
// click at any time (including just to check status), and calling
// CreateToken unconditionally would downgrade a perfectly live session back
// to PENDING, forcing a needless re-approval.
func (e *WebullExchange) Reconnect(ctx context.Context) (string, error) {
	if status, expiresAt := e.adapter.client.SessionInfo(); status == TokenStatusNormal &&
		time.Now().Add(60*time.Second).Before(expiresAt) {
		return status, nil
	}
	resp, err := e.adapter.client.CreateToken(ctx)
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// CheckReauth implements exchanges.ReauthExchange by polling
// token/check once for whether a pending login has been approved in the
// Webull mobile app.
func (e *WebullExchange) CheckReauth(ctx context.Context) (string, error) {
	resp, err := e.adapter.client.CheckToken(ctx)
	if err != nil {
		return "", err
	}
	return resp.Status, nil
}

// SessionInfo implements exchanges.SessionInfoExchange.
func (e *WebullExchange) SessionInfo() (string, time.Time) {
	return e.adapter.client.SessionInfo()
}
