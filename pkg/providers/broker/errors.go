package broker

import "errors"

// Sentinel errors returned by broker providers.
var (
	ErrSymbolNotFound    = errors.New("broker: symbol not found")
	ErrInsufficientFunds = errors.New("broker: insufficient funds")
	ErrOrderRejected     = errors.New("broker: order rejected")
	ErrRateLimited       = errors.New("broker: rate limit exceeded")
)
