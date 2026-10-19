package proxyenv

import (
	"net"
	"os"
)

// Listener is bound during package init — before any transitive dependency
// (such as github.com/modelcontextprotocol/go-sdk/oauthex) can call
// http.ProxyFromEnvironment and memoize the proxy environment variables.
var Listener net.Listener

func init() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	Listener = ln
	proxyURL := "http://" + ln.Addr().String()
	_ = os.Setenv("HTTP_PROXY", proxyURL)
	_ = os.Setenv("HTTPS_PROXY", proxyURL)
}
