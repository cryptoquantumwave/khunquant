package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRouterHandlesOutOfRangeFixtureStatus covers a fixture whose status code is
// not a legal HTTP status. Nothing on the write path rejects it — neither the
// MCP sandbox_upsert_fixture handler nor the gateway's PUT endpoint constrains
// the value — so it reaches http.ResponseWriter.WriteHeader, which panics on
// codes outside 100..999.
func TestRouterHandlesOutOfRangeFixtureStatus(t *testing.T) {

	for _, status := range []int{99999, 0, -1} {
		t.Run(http.StatusText(status)+itoa(status), func(t *testing.T) {
			store := NewStore()
			store.SetFixtures("binance", []FixtureEntry{{
				Method:     "GET",
				PathPrefix: "/api/v3/account",
				Response:   Response{Status: status, Body: []byte(`{"ok":true}`)},
			}})

			handler := BuildRouter(store)
			req := httptest.NewRequest("GET", "/__sbx__/binance/api.binance.com/api/v3/account", nil)
			rec := httptest.NewRecorder()

			defer func() {
				if r := recover(); r != nil {
					t.Errorf("serving a fixture with status %d panicked: %v; "+
						"the status is accepted at write time but is not a legal "+
						"HTTP status code", status, r)
				}
			}()

			handler(rec, req)
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
