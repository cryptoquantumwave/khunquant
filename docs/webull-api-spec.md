# Webull OpenAPI Specification

**Last Updated:** 2026-07-10  
**Status:** Ground-truth reference for Go integration  
**Scope:** Trading API + Market Data API

---

## Confirmed Constants

- **Signing (authentication/signature.md)**: Headers involved in signing = x-app-key, x-timestamp, x-signature-algorithm, x-signature-version, x-signature-nonce, host. Excluded from signing: x-signature, x-version. Build str1 = sorted `name=value&name=value` (PLAIN, not per-component encoded). str2 = toUpper(hex(MD5(body))) if body present. str3 = path + "&" + str1 (+ "&" + str2 if body). Then **URL-encode the ENTIRE str3**. key = app_secret + "&". signature = base64(HMAC-SHA1(key, urlencoded_str3)). Timestamp = ISO8601 `YYYY-MM-DDThh:mm:ssZ` UTC. Nonce = uuid4().hex (32 hex chars).

- **Hosts**: 
  - Prod trading+marketdata HTTP = `api.webull.com`
  - Sandbox = `api.sandbox.webull.com`
  - Market data client-to-server = `api.webull.com` (prod) — **NOT global.webullsolutions.com** (spec was incorrect in MEMORY.md)
  - regionId = "us"

- **Token flow**: POST /openapi/auth/token/create (signed, no body) → {token (32-hex), expires (unix ms), status: PENDING|NORMAL|INVALID|EXPIRED}. Sandbox tokens valid by default (no SMS). Prod: SMS-verify once, valid 15 days, reuse/refresh.

- **All trade-api endpoints** require header `x-access-token` (from token flow) PLUS the signing headers + `x-version: v2`.

- **Market data subscription**: **Required** for accessing both historical and real-time bars/snapshot/quotes for US stocks and ETFs. Auth via app-key/secret S2S (same signing method as trade-api).

---

## Authentication Endpoints

### POST /openapi/auth/token/check
**Host:** `api.sandbox.webull.com` (also prod = `api.webull.com`)  
**Rate Limit:** 10 requests per 30 seconds

#### Request Headers (all required)
```
x-app-key: string (Developer API identifier)
x-app-secret: string (Developer API key)
x-timestamp: string (ISO8601 UTC: YYYY-MM-DDThh:mm:ssZ)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string (Random unique number)
x-version: string (Must be "v2")
x-signature: string (Digital fingerprint)
Content-Type: application/json
```

#### Request Body
```json
{
  "token": "string (required) - 32-digit hexadecimal access token for identity authentication"
}
```

#### Response (200 OK)
```json
{
  "token": "string",
  "expires": "integer (int64, Unix milliseconds)",
  "status": "enum [PENDING, NORMAL, INVALID, EXPIRED]"
}
```

#### Error Responses
- **401:** Unauthorized (insufficient permission)
- **417:** Business logic error (invalid parameters)
- **500:** Internal Server Error

---

## Trading Endpoints

### POST /openapi/trade/order/place
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 600 requests per minute

#### Request Headers (all required)
```
x-app-key: string
x-app-secret: string
x-timestamp: string (ISO8601 UTC)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string (Authorization credential)
x-version: string (Must be "v2")
x-signature: string
Content-Type: application/json
```

#### Request Body Schema
```json
{
  "account_id": "string (required)",
  "client_combo_order_id": "string (optional)",
  "new_orders": [
    {
      "client_order_id": "string (required, max 32 chars, must be unique per account)",
      "combo_type": "string (enum: NORMAL, MASTER, STOP_PROFIT, STOP_LOSS, OTO, OCO, OTOCO)",
      "entrust_type": "string (enum: QTY, AMOUNT - required)",
      "instrument_type": "string (enum: EQUITY, OPTION, FUTURES, CRYPTO, EVENT - required)",
      "market": "string (enum: US - required)",
      "order_type": "string (enum: MARKET, LIMIT, STOP_LOSS, STOP_LOSS_LIMIT, TRAILING_STOP_LOSS, MARKET_ON_OPEN, MARKET_ON_CLOSE, LIMIT_ON_OPEN - required)",
      "side": "string (enum: BUY, SELL, SHORT - required)",
      "symbol": "string (required)",
      "time_in_force": "string (enum: DAY, GTC, IOC, GTD, FOK - required)",
      "quantity": "string (optional - for entrust_type=QTY, supports decimals)",
      "total_cash_amount": "string (optional - for entrust_type=AMOUNT)",
      "limit_price": "string (optional)",
      "stop_price": "string (optional)",
      "expire_date": "string (format: yyyy-MM-dd, optional)",
      "support_trading_session": "string (enum: ALL, CORE, NIGHT - optional)",
      "trailing_type": "string (enum: AMOUNT, PERCENTAGE - optional)",
      "trailing_stop_step": "string (optional)",
      "current_ask": "string (optional)",
      "current_bid": "string (optional)",
      "algo_type": "string (enum: TWAP, VWAP, POV - optional)",
      "target_vol_percent": "string (optional)",
      "max_target_percent": "string (optional)",
      "algo_start_time": "string (format: HH:mm:ss - optional)",
      "algo_end_time": "string (format: HH:mm:ss - optional)",
      "option_strategy": "string (enum: SINGLE, COVERED_STOCK, STRADDLE, STRANGLE, VERTICAL, CALENDAR, BUTTERFLY, CONDOR, COLLAR_WITH_STOCK, IRON_BUTTERFLY, IRON_CONDOR, DIAGONAL - optional)",
      "event_outcome": "string (enum: yes, no - optional)",
      "event_trade_mode": "string (enum: TRADE_IN_AMOUNT, TRADE_IN_CONTRACT - optional)",
      "legs": [
        {
          "side": "string (enum: BUY, SELL, SHORT - required)",
          "quantity": "string (optional)",
          "market": "string (enum: US - required)",
          "instrument_type": "string (enum: EQUITY, OPTION - required)",
          "symbol": "string (required)",
          "strike_price": "string (optional)",
          "option_expire_date": "string (format: yyyy-MM-dd - optional)",
          "option_type": "string (enum: CALL, PUT - optional)"
        }
      ]
    }
  ]
}
```

#### Response (200 OK)
```json
{
  "client_order_id": "string",
  "order_id": "string"
}
```

#### Error Responses
- **401:** Authentication failure
- **417:** Business validation error
- **500:** Server error

---

### POST /openapi/trade/order/preview
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 150 requests per 10 seconds

#### Request Headers
Same as `/order/place`

#### Request Body Schema
Same as `/order/place` (new_orders array)

#### Response (200 OK)
```json
{
  "estimated_cost": "string (Capital required; for stocks/options includes consideration; for futures indicates initial margin)",
  "estimated_transaction_fee": "string (Estimated fees including exchange, clearing, and commission charges)"
}
```

#### Error Responses
- **401:** Unauthorized
- **417:** Business error
- **500:** Server error

---

### POST /openapi/trade/order/replace
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 600 requests per minute

#### Request Headers
Same as `/order/place`

#### Request Body Schema
```json
{
  "account_id": "string (required)",
  "modify_orders": [
    {
      "client_order_id": "string (required, max 32 chars)",
      "time_in_force": "string (optional: DAY, GTC, IOC, GTD, FOK)",
      "order_type": "string (optional: MARKET, LIMIT, STOP_LOSS, STOP_LOSS_LIMIT, TRAILING_STOP_LOSS)",
      "quantity": "string (optional, supports decimals)",
      "limit_price": "string (optional)",
      "stop_price": "string (optional)",
      "trailing_type": "string (optional: AMOUNT, PERCENTAGE)",
      "trailing_stop_step": "string (optional)",
      "target_vol_percent": "string (optional, 1-20)",
      "max_target_percent": "string (optional, 1-20)",
      "algo_start_time": "string (optional, HH:mm:ss ET)",
      "algo_end_time": "string (optional, HH:mm:ss ET)",
      "legs": [
        {
          "id": "string (required)",
          "quantity": "string (required)"
        }
      ]
    }
  ]
}
```

#### Response (200 OK)
```json
{
  "client_order_id": "string",
  "client_combo_order_id": "string",
  "combo_order_id": "string",
  "order_id": "string"
}
```

#### Error Responses
- **401:** Authentication failure
- **417:** Business validation error
- **500:** Server error

---

### POST /openapi/trade/order/cancel
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** Not specified (assume standard trade limit)

#### Request Headers
Same as `/order/place`

#### Request Body Schema
```json
{
  "account_id": "string (required)",
  "client_order_id": "string (required, max 32 chars)"
}
```

#### Response (200 OK)
```json
{
  "client_order_id": "string",
  "client_combo_order_id": "string",
  "combo_order_id": "string",
  "order_id": "string"
}
```

#### Error Responses
- **401:** Unauthorized
- **417:** Business error
- **500:** Server error

---

### GET /openapi/trade/order/open
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 2 requests per 2 seconds

#### Query Parameters
```
account_id: string (required) - Account identifier
page_size: string (optional, default 10, max 100) - Records per query
last_client_order_id: string (optional) - Cursor for pagination; omit on first request
```

#### Request Headers
Same as `/order/place` (minus Content-Type if no body)

#### Response (200 OK)
```json
[
  {
    "combo_type": "string (NORMAL|MASTER|STOP_PROFIT|STOP_LOSS|OTO|OCO|OTOCO)",
    "combo_order_id": "string",
    "orders": [
      {
        "client_order_id": "string",
        "order_id": "string",
        "symbol": "string",
        "side": "string (BUY|SELL|SHORT)",
        "status": "string (PENDING|SUBMITTED|CANCELLED|FILLED|FAILED|PARTIAL_FILLED)",
        "order_type": "string (MARKET|LIMIT|STOP_LOSS|STOP_LOSS_LIMIT|TRAILING_STOP_LOSS|MARKET_ON_OPEN|MARKET_ON_CLOSE|LIMIT_ON_OPEN)",
        "instrument_type": "string (EQUITY|OPTION|FUTURES|CRYPTO|EVENT)",
        "entrust_type": "string (QTY|AMOUNT)",
        "time_in_force": "string (DAY|GTC|IOC|GTD|FOK)",
        "total_quantity": "string",
        "filled_quantity": "string",
        "filled_price": "string",
        "limit_price": "string",
        "stop_price": "string",
        "place_time": "string (milliseconds since epoch)",
        "place_time_at": "string (ISO8601 UTC)",
        "filled_time": "string (milliseconds)",
        "filled_time_at": "string (ISO8601 UTC)",
        "algo_type": "string (TWAP|VWAP|POV)",
        "legs": [
          {
            "id": "string",
            "symbol": "string",
            "side": "string (BUY|SELL|SHORT)",
            "quantity": "string",
            "option_type": "string (CALL|PUT)",
            "option_category": "string (AMERICAN|EUROPEAN)",
            "strike_price": "string",
            "option_expire_date": "string (yyyy-MM-dd)"
          }
        ]
      }
    ]
  }
]
```

#### Error Responses
- **401:** Authentication failure
- **417:** Business error
- **500:** Server error

---

### GET /openapi/trade/order/history
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 2 requests per 2 seconds

#### Query Parameters
```
account_id: string (required) - Account identifier
start_date: string (optional, format yyyy-MM-dd) - Start of query period (defaults to last 7 days)
end_date: string (optional, format yyyy-MM-dd) - End of query period (defaults to last 7 days)
page_size: string (optional, default 10, max 100) - Records per query
last_client_order_id: string (optional) - Cursor for pagination
```

#### Request Headers
Same as `/order/open`

#### Response (200 OK)
```json
[
  {
    "combo_type": "string (NORMAL|MASTER|STOP_PROFIT|STOP_LOSS|OTO|OCO|OTOCO)",
    "client_order_id": "string",
    "combo_order_id": "string",
    "orders": [
      {
        "client_order_id": "string",
        "order_id": "string",
        "symbol": "string",
        "side": "string (BUY|SELL|SHORT)",
        "status": "string (PENDING|SUBMITTED|CANCELLED|FILLED|FAILED|PARTIAL_FILLED)",
        "order_type": "string (MARKET|LIMIT|STOP_LOSS|STOP_LOSS_LIMIT|TRAILING_STOP_LOSS|MARKET_ON_OPEN|MARKET_ON_CLOSE|LIMIT_ON_OPEN)",
        "instrument_type": "string (EQUITY|OPTION|FUTURES|CRYPTO|EVENT)",
        "entrust_type": "string (QTY|AMOUNT)",
        "time_in_force": "string (DAY|GTC|IOC|GTD|FOK)",
        "total_quantity": "string",
        "filled_quantity": "string",
        "filled_price": "string",
        "limit_price": "string",
        "stop_price": "string",
        "place_time": "string (milliseconds)",
        "place_time_at": "string (ISO8601)",
        "filled_time": "string (milliseconds)",
        "filled_time_at": "string (ISO8601)",
        "legs": [
          {
            "id": "string",
            "symbol": "string",
            "side": "string",
            "quantity": "string",
            "option_type": "string (CALL|PUT)",
            "option_category": "string (AMERICAN|EUROPEAN)",
            "strike_price": "string",
            "option_expire_date": "string (yyyy-MM-dd)"
          }
        ]
      }
    ]
  }
]
```

#### Error Responses
- **401:** Unauthorized
- **417:** Business error
- **500:** Server error

---

### GET /openapi/trade/order/detail
**Host:** `api.sandbox.webull.com`  
**Rate Limit:** 2 requests per 2 seconds

#### Query Parameters
```
account_id: string (required) - Account identifier
client_order_id: string (required) - The order ID to fetch details for (used for cursor-based pagination)
```

#### Request Headers
Same as `/order/open`

#### Response (200 OK)
```json
{
  "combo_type": "string (NORMAL|MASTER|STOP_PROFIT|STOP_LOSS|OTO|OCO|OTOCO)",
  "client_order_id": "string",
  "combo_order_id": "string",
  "orders": [
    {
      "client_order_id": "string",
      "order_id": "string",
      "symbol": "string",
      "side": "string (BUY|SELL|SHORT)",
      "status": "string (PENDING|SUBMITTED|CANCELLED|FILLED|FAILED|PARTIAL_FILLED)",
      "order_type": "string (MARKET|LIMIT|STOP_LOSS|STOP_LOSS_LIMIT|TRAILING_STOP_LOSS|MARKET_ON_OPEN|MARKET_ON_CLOSE|LIMIT_ON_OPEN)",
      "instrument_type": "string (EQUITY|OPTION|FUTURES|CRYPTO|EVENT)",
      "entrust_type": "string (QTY|AMOUNT)",
      "time_in_force": "string (DAY|GTC|IOC|GTD|FOK)",
      "total_quantity": "string",
      "filled_quantity": "string",
      "filled_price": "string",
      "limit_price": "string",
      "stop_price": "string",
      "place_time": "string (milliseconds)",
      "place_time_at": "string (ISO8601)",
      "filled_time": "string (milliseconds)",
      "filled_time_at": "string (ISO8601)",
      "legs": [
        {
          "id": "string",
          "symbol": "string",
          "side": "string (BUY|SELL|SHORT)",
          "quantity": "string",
          "option_type": "string (CALL|PUT)",
          "option_category": "string (AMERICAN|EUROPEAN)",
          "strike_price": "string",
          "option_expire_date": "string (yyyy-MM-dd)"
        }
      ],
      "commission": {
        "actual_commission": "string",
        "receivable_commission": "string"
      },
      "fees": [
        {
          "type": "string",
          "actual_value": "string",
          "receivable_value": "string"
        }
      ]
    }
  ]
}
```

#### Error Responses
- **401:** Authentication failure
- **417:** Business error
- **500:** Server error

---

## Market Data Endpoints

### GET /openapi/market-data/stock/bars
**Host:** `api.sandbox.webull.com` (prod: `api.webull.com`)  
**Rate Limit:** 600 requests per minute  
**Requires Subscription:** Yes (market data subscription mandatory)

#### Query Parameters
```
symbol: string (required) - Single security symbol (e.g., AAPL)
category: string (required) - enum: US_STOCK, US_ETF
timespan: string (required) - enum: M1, M5, M15, M30, M60, M120, M240, D, W, M, Y
count: string (optional, default 200) - Range 1-1200 (M1: 1-1650)
real_time_required: string (required) - enum: true, false (default true)
trading_sessions: string (optional) - enum: PRE, RTH, ATH, OVN (can be comma-separated)
start_time: integer (optional, int64) - Millisecond Unix timestamp
end_time: integer (optional, int64) - Millisecond Unix timestamp
```

#### Request Headers
```
x-app-key: string (Developer API identifier)
x-app-secret: string (Developer API key)
x-timestamp: string (ISO8601 UTC: YYYY-MM-DDThh:mm:ssZ)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string (Authorization credential)
x-version: string (Must be "v2")
x-signature: string (Digital fingerprint)
```

#### Response (200 OK)
```json
[
  {
    "tickerId": "string",
    "symbol": "string",
    "time": "string (UTC ISO8601)",
    "open": "string",
    "close": "string",
    "high": "string",
    "low": "string",
    "volume": "string (optional)",
    "trading_session": "string (optional, PRE|RTH|ATH|OVN)"
  }
]
```

#### Error Responses
- **401:** Unauthorized
- **417:** Business error (invalid parameter)
- **500:** Server error

---

### POST /openapi/market-data/stock/batch-bars
**Host:** `api.sandbox.webull.com` (prod: `api.webull.com`)  
**Rate Limit:** 600 requests per minute  
**Requires Subscription:** Yes

#### Request Headers
```
x-app-key: string
x-app-secret: string
x-timestamp: string (ISO8601 UTC)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string
x-version: string (Must be "v2")
x-signature: string
Content-Type: application/json
```

#### Request Body Schema
```json
{
  "symbols": ["string", "string", "..."] (array, max 20 symbols, required),
  "category": "string (enum: US_STOCK, US_ETF, required)",
  "timespan": "string (enum: M1, M5, M15, M30, M60, M120, M240, D, W, M, Y, required)",
  "count": "integer (optional, default 200, max 1200 or 1650 for M1)",
  "real_time_required": "boolean (optional, default true)",
  "trading_sessions": "string (optional, enum: PRE, RTH, ATH, OVN, comma-separated)",
  "start_time": "integer (optional, int64 milliseconds)",
  "end_time": "integer (optional, int64 milliseconds)"
}
```

#### Response (200 OK)
```json
{
  "result": [
    {
      "symbol": "string",
      "instrument_id": "string",
      "result": [
        {
          "time": "string (ISO8601)",
          "open": "string",
          "close": "string",
          "high": "string",
          "low": "string",
          "volume": "string"
        }
      ]
    }
  ]
}
```

#### Error Responses
- **401:** Unauthorized
- **417:** Business error
- **500:** Server error

---

### GET /openapi/market-data/stock/snapshot
**Host:** `api.sandbox.webull.com` (prod: `api.webull.com`)  
**Rate Limit:** 600 requests per minute  
**Requires Subscription:** Yes

#### Query Parameters
```
symbols: string (required) - List of security symbols; supports JSON array format, multiple symbols separated by commas; maximum 100 symbols per query
category: string (required) - enum: US_STOCK, US_ETF
extend_hour_required: string (optional, default false) - Include pre/post-market data
overnight_required: string (optional, default false) - Include overnight trading data
```

#### Request Headers
```
x-app-key: string
x-app-secret: string
x-timestamp: string (ISO8601 UTC)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string
x-version: string (Must be "v2")
x-signature: string
```

#### Response (200 OK)
```json
[
  {
    "symbol": "string",
    "instrument_id": "string",
    "price": "string",
    "pre_close": "string",
    "open": "string",
    "high": "string",
    "low": "string",
    "volume": "string",
    "change": "string",
    "change_ratio": "string",
    "last_trade_time": "integer (Unix timestamp ms)",
    "bid": "string",
    "bid_size": "string",
    "ask": "string",
    "ask_size": "string",
    "turnover": "string",
    "eps": "string",
    "eps_ttm": "string",
    "bps": "string",
    "lot_size": "string",
    "extend_hour_price": "string (optional)",
    "extend_hour_high": "string (optional)",
    "extend_hour_low": "string (optional)",
    "extend_hour_change": "string (optional)",
    "extend_hour_volume": "string (optional)",
    "ovn_price": "string (optional)",
    "ovn_high": "string (optional)",
    "ovn_low": "string (optional)",
    "ovn_change": "string (optional)",
    "ovn_volume": "string (optional)"
  }
]
```

#### Error Responses
- **401:** Unauthorized
- **417:** Invalid parameter
- **500:** Server error

---

### GET /openapi/market-data/stock/quotes
**Host:** `api.sandbox.webull.com` (prod: `api.webull.com`)  
**Rate Limit:** 600 requests per minute  
**Requires Subscription:** Yes

#### Query Parameters
```
symbol: string (required) - Single security symbol (e.g., GOOG)
category: string (required) - enum: US_STOCK, US_ETF
depth: string (required) - Market depth level (L1, L2 with default 10, etc.)
overnight_required: string (required) - enum: true, false (default false) - Include overnight trading
```

#### Request Headers
```
x-app-key: string
x-app-secret: string
x-timestamp: string (ISO8601 UTC)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string
x-version: string (Must be "v2")
x-signature: string
```

#### Response (200 OK)
```json
{
  "symbol": "string",
  "instrument_id": "string",
  "quote_time": "string",
  "asks": [
    {
      "price": "string",
      "size": "string",
      "order": [
        {
          "mpid": "string",
          "size": "string"
        }
      ],
      "broker": [
        {
          "bid": "string",
          "name": "string"
        }
      ]
    }
  ],
  "bids": [
    {
      "price": "string",
      "size": "string",
      "order": [
        {
          "mpid": "string",
          "size": "string"
        }
      ],
      "broker": [
        {
          "bid": "string",
          "name": "string"
        }
      ]
    }
  ]
}
```

#### Error Responses
- **401:** Unauthorized
- **417:** Invalid parameter
- **500:** Server error

---

## Instrument Endpoints

### GET /openapi/instrument/stock/list
**Host:** `api.sandbox.webull.com` (prod: `api.webull.com`)  
**Rate Limit:** 60 requests per 60 seconds

#### Query Parameters
```
symbols: string (optional) - List of security symbols; max 100 per query (e.g., AAPL,TSLA)
category: string (required) - enum: US_STOCK only
status: string (optional) - Tradable status; enum: OC (tradable), CO (liquidate only), NT (non-tradable)
last_instrument_id: string (optional) - Pagination cursor using last instrument ID returned
page_size: integer (optional, default 1000) - Results per page
```

#### Request Headers
```
x-app-key: string
x-app-secret: string
x-timestamp: string (ISO8601 UTC)
x-signature-version: string (default "1.0")
x-signature-algorithm: string (default "HMAC-SHA1")
x-signature-nonce: string
x-access-token: string
x-version: string (Must be "v2")
x-signature: string
```

#### Response (200 OK)
```json
[
  {
    "instrument_id": "string (unique identifier)",
    "symbol": "string (e.g., AAPL)",
    "name": "string (e.g., APPLE INC)",
    "exchange_code": "string (e.g., NSQ)",
    "category": "string (US_STOCK)",
    "status": "string (OC|CO|NT)",
    "shortable": "boolean",
    "fractionable": "boolean",
    "marginable": "boolean",
    "overnight_trading_supported": "boolean",
    "margin_requirement_long": "string (decimal ratio)",
    "margin_requirement_short": "string (decimal ratio)",
    "intraday_margin_long": "string",
    "intraday_margin_short": "string",
    "maintenance_margin_long": "string",
    "maintenance_margin_short": "string",
    "easy_to_borrow": "boolean",
    "lot_size": "string (e.g., 1.0)",
    "currency": "string (e.g., USD)"
  }
]
```

#### Symbol Resolution
Pass ticker symbols via the `symbols` query parameter; response includes corresponding `instrument_id` for each symbol.

#### Error Responses
- **401:** Unauthorized
- **417:** Business error
- **500:** Server error

---

## Stock-Specific Order Rules

### Fractional Share Trading
- **Quantity supports decimals** (e.g., 0.5 shares per order)
- **Only MARKET orders** are supported for fractional trading
- **Range:** between 0 (exclusive) and 1 (inclusive) per order
- **Minimum order value:** $5
- **Fractional trading field:** `fractionable: boolean` in instrument response

### Quantity vs Amount Parameters
- **Standard orders:** Use `quantity` parameter (supports decimals for fractional shares)
- **Amount-based orders:** When `entrust_type: "AMOUNT"`, use `total_cash_amount` instead of quantity
- **Amount constraint:** Must be less than the price of 1 share (to enable fractional share purchases via dollar amounts)

### Stock Order Type Constraints
- `instrument_type` must be `"EQUITY"` (not FUTURES/CRYPTO/etc.)
- `market` must be `"US"`
- `entrust_type` options: `"QTY"` (supports fractional decimals) or `"AMOUNT"`
- `time_in_force` for stocks: `"DAY"` or `"GTC"` (other values like IOC, GTD, FOK may fail on stocks)
- `order_type` values for stocks: MARKET, LIMIT, STOP_LOSS, STOP_LOSS_LIMIT, TRAILING_STOP_LOSS, MARKET_ON_OPEN, MARKET_ON_CLOSE, LIMIT_ON_OPEN
- `client_order_id` maximum: 32 characters; must be unique per account
- **Trailing stops** only support `time_in_force: "DAY"`
- **Algorithmic orders** run only during regular trading hours (RTH)

---

## Timeframe Enum Reference

Used in bars endpoints (both single and batch):

```
M1    = 1 minute
M5    = 5 minutes
M15   = 15 minutes
M30   = 30 minutes
M60   = 60 minutes (1 hour)
M120  = 120 minutes (2 hours)
M240  = 240 minutes (4 hours)
D     = Daily
W     = Weekly
M     = Monthly
Y     = Yearly
```

**NOTE:** Webull uses `M*` notation (e.g., `M1`, `M5`), NOT lowercase ISO notation (e.g., `1m`, `5m`).

---

## ✅ Empirically Verified (2026-07-10, sandbox host `us-openapi-alb.uat.webullbroker.com`, shared cred #1)

All calls returned **HTTP 200** with the Go signer described above (whole-string URL-encode; GET and POST-with-body both proven):

| Endpoint | Result |
|---|---|
| POST /openapi/auth/token/create | 200 — token usable at status=PENDING in sandbox |
| GET /openapi/account/list | 200 — cash + margin accounts |
| GET /openapi/assets/balance | 200 |
| GET /openapi/assets/positions | 200 — see real fields below |
| GET /openapi/instrument/stock/list | 200 — AAPL → instrument_id 913256135, fractionable:true |
| GET /openapi/market-data/stock/snapshot | 200 — **market data works on sandbox creds, no subscription block** |
| GET /openapi/market-data/stock/bars (timespan=D) | 200 — OHLCV, `time`="2026-07-09T04:00:00.000+0000" |
| POST /openapi/trade/order/preview | 200 — `{estimated_cost, estimated_transaction_fee, currency}` (body-signing/MD5 path proven) |

**Order lifecycle verified (place→open→detail→cancel, all HTTP 200):**
- `POST /openapi/trade/order/place` → `{client_order_id, order_id}` (NO status field in response).
- `GET /openapi/trade/order/open` & `/detail` return combo objects `{client_order_id, combo_type, combo_order_id, orders:[{symbol, side, status, client_order_id, order_id, order_type, instrument_type, entrust_type, time_in_force, total_quantity, filled_quantity, limit_price, support_trading_session, place_time(ms str), place_time_at(ISO8601), fees:[], commission:{}}]}`. `open` = array; `detail` = single object. `combo_order_id == order_id` for NORMAL single orders.
- `POST /openapi/trade/order/cancel` body `{account_id, client_order_id}` → `{client_order_id, order_id}`.
- **`client_order_id` is the round-trip key** for detail/cancel (NOT order_id) → expose it as `ccxt.Order.Id`.
- Resting LIMIT order status = `SUBMITTED` (→ ccxt "open").

## ✅ Options + ETF recon (2026-07-10, same sandbox) — for the options/ETF phase

- **Options TRADING single-leg — VERIFIED (place→open→cancel all 200).** Order body:
  `{account_id, new_orders:[{client_order_id, combo_type:NORMAL, option_strategy:SINGLE,
  order_type:LIMIT, limit_price, quantity, side:BUY, time_in_force:DAY, entrust_type:QTY,
  instrument_type:OPTION, market:US, symbol:<underlying>, legs:[{side,quantity,symbol:<underlying>,
  strike_price,option_expire_date(yyyy-MM-dd),instrument_type:OPTION,option_type:CALL|PUT,
  market:US}]}]}`. Response `{client_order_id, order_id}`.
  - `support_trading_session` is **NOT required for options** (I omitted it; server defaulted `CORE`). Unlike equities.
  - `open`/`detail` leg echo adds: `option_category:AMERICAN`, **`option_contract_multiplier:"100"`**,
    `option_contract_deliverable:"100"`, `expiration_type:PM`, `option_strategy:SINGLE`,
    `position_intent:BUY_TO_OPEN` (server-inferred — don't send).
  - **Cost = limit_price × 100 × qty** (multiplier 100): preview `estimated_cost="100.00"` for qty1 @ limit 1.00.
  - Order types LIMIT|STOP_LOSS|STOP_LOSS_LIMIT (no MARKET); TIF DAY|GTC (SELL day-only, GTC buy-only).
- **Option MARKET DATA (snapshot/greeks) is SUBSCRIPTION-GATED:** `401 "Insufficient permission,
  please subscribe to US_OPTION quotes."` — cannot verify greeks in sandbox; needs a US_OPTION
  market-data subscription in prod. Implement per docs; document the subscription requirement.
- **ETF market data NOT sandbox-verifiable:** sandbox market data is restricted to AAPL only
  (`403 INVALID_SYMBOL "Only AAPL is allowed"` for SPY under both US_STOCK and US_ETF). Implement
  the US_STOCK→US_ETF category fallback per docs; verify against a real (prod) account.

**Refinements to record:**
- **`support_trading_session` is REQUIRED for equity orders** (value `CORE`); omitting → 417 `OAUTH_OPENAPI_PARAM_ERR "invalid support_trading_session"`. Order enum = ALL|CORE|NIGHT (distinct from market-data `trading_sessions` = PRE|RTH|ATH|OVN).
- **Positions real DTO** (`/openapi/assets/positions`): `{currency, quantity, cost, proportion, position_id, symbol, instrument_type, cost_price, last_price, market_value, unrealized_profit_loss, unrealized_profit_loss_rate, day_profit_loss, day_realized_profit_loss}`.
- **Snapshot real DTO** includes: price, open, high, low, close, pre_close, volume, change, change_ratio, ask/ask_size, bid/bid_size, pe_ratio, pb_ratio, ps_ratio, market_value, fifty_two_wk_high/low, eps, bps, last_trade_time(ms), quote_time(ms), list_status.
- **Bars real DTO**: `{tickerId, symbol, time(ISO8601 +0000), open, close, high, low, volume, trading_session}`. Newest-first order.
- **Preview 200 body** also includes `currency` (spec listed only estimated_cost + estimated_transaction_fee).
- Reference-doc "Base URL: api.sandbox.webull.com" is WRONG for shared creds → use `us-openapi-alb.uat.webullbroker.com` for sandbox.

## Open Questions / Gaps

1. **Market data host in prod:** Confirmed as `api.webull.com` (HTTP), not `global.webullsolutions.com`. MEMORY.md was incorrect.

2. **data-api.webull.com (MQTT):** Spec mentions `data-api.webull.com` for MQTT; Go integration currently uses REST bars endpoints, so MQTT connectivity is out of scope.

3. **Batch bars max count for M1:** Docs state "M1: 1-1650" but standard max is 1200. Clarify exact limit per timespan.

4. **Depth parameter in quotes:** The `depth` parameter is required but docs are vague on exact enum values. L1 and L2 are mentioned; unclear if other depths (L3, etc.) are supported.

5. **Trading session values in bars:** `trading_sessions` can be PRE, RTH, ATH, OVN. Unclear which combinations are valid or if comma-separated list is correctly supported by API.

6. **Commission and fees response:** `order/detail` includes commission and fees fields with `actual_value` and `receivable_value`. Unclear which represents the actual amount charged vs projected.

7. **Combo order semantics:** Docs mention `combo_type` (NORMAL, MASTER, STOP_PROFIT, STOP_LOSS, OTO, OCO, OTOCO) but don't specify legs requirements per combo type or how client_combo_order_id is used.

8. **Pagination cursor behavior:** Both order history and order open use `last_client_order_id` for pagination. Docs don't clarify if it's inclusive/exclusive or if cursor must be the actual client_order_id or a server-opaque token.

9. **algo_type constraints:** TWAP/VWAP/POV are mentioned but no param details (e.g., target_vol_percent range, algo_start_time format relative to market hours).

10. **Market data subscription model:** Docs confirm subscription is required but don't specify pricing tiers, trial periods, or scope (US-stock-only vs all asset classes).

---

## Implementation Notes for Go

1. **Timeframe mapping:** Create an enum or const mapping `M1, M5, M15, M30, M60, M120, M240, D, W, M, Y` (exact string match; no case conversion).

2. **Signature building:** URL-encode the entire `str3` (path + sorted params + optional body MD5 hash), not per-component. This is critical.

3. **Market data is a paid service:** Sandbox accounts may have free access; production requires active subscription.

4. **Fractional shares for stocks:** Default to whole shares unless explicitly passing a decimal quantity and confirming the instrument has `fractionable: true`.

5. **Order types vary by instrument:** Always validate `instrument_type`, `order_type`, and `time_in_force` combinations against this spec.

6. **Response arrays:** All order list endpoints (open, history, detail) return arrays of combo order objects, not flat order arrays. Flatten as needed in client code.

7. **Pagination:** Use cursor-based pagination (last ID) for all list endpoints. Do not rely on offset/limit.

8. **Rate limits:** Market data and trading have different buckets (600/min vs 2/2sec or 600/min). Implement separate rate-limit tracking per endpoint family.

---

**Document Generated:** 2026-07-10  
**Next Review:** After next Webull API release or when integration deviates from spec
