package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// effectiveGatewayBindHost reports the host the gateway binds to, which is what
// gateway.host says and nothing else.
//
// H-10: the launcher used to override this with 0.0.0.0 whenever the launcher
// itself was started with -public, so a flag about the launcher's own listen
// address silently moved the gateway - webhooks, pico - onto the LAN. An
// operator who wants that now says so explicitly, by setting gateway.host or
// the KHUNQUANT_GATEWAY_HOST environment variable it is bound to.
func (h *Handler) effectiveGatewayBindHost(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Gateway.Host)
}

func gatewayProbeHost(bindHost string) string {
	if bindHost == "" || bindHost == "0.0.0.0" {
		return "127.0.0.1"
	}
	return bindHost
}

func requestHostName(r *http.Request) string {
	reqHost, _, err := net.SplitHostPort(r.Host)
	if err == nil {
		return reqHost
	}
	if strings.TrimSpace(r.Host) != "" {
		return r.Host
	}
	return "127.0.0.1"
}

func forwardedProtoFirst(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if raw == "" {
		raw = forwardedRFC7239Proto(r)
	}
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return strings.ToLower(raw)
}

func requestWSScheme(r *http.Request) string {
	if forwarded := forwardedProtoFirst(r); forwarded != "" {
		proto := strings.ToLower(strings.TrimSpace(strings.Split(forwarded, ",")[0]))
		if proto == "https" || proto == "wss" {
			return "wss"
		}
		if proto == "http" || proto == "ws" {
			return "ws"
		}
	}

	if r.TLS != nil {
		return "wss"
	}

	return "ws"
}

// requestHTTPScheme returns http or https for URLs that are not WebSockets (e.g. SSE).
func requestHTTPScheme(r *http.Request) string {
	if forwarded := forwardedProtoFirst(r); forwarded != "" {
		proto := strings.ToLower(strings.TrimSpace(strings.Split(forwarded, ",")[0]))
		if proto == "https" || proto == "wss" {
			return "https"
		}
		if proto == "http" || proto == "ws" {
			return "http"
		}
	}
	if r.TLS != nil {
		return "https"
	}

	return "http"
}

// forwardedHostFirst returns the client-visible host from reverse-proxy / tunnel headers
// (e.g. VS Code port forwarding, nginx). Empty if unset.
func forwardedHostFirst(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if raw == "" {
		raw = forwardedRFC7239Host(r)
	}
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw
}

// forwardedRFC7239Host parses host= from the first Forwarded header element (RFC 7239).
func forwardedRFC7239Host(r *http.Request) string {
	return forwardedRFC7239Param(r, "host")
}

func forwardedRFC7239Proto(r *http.Request) string {
	return forwardedRFC7239Param(r, "proto")
}

func forwardedRFC7239Param(r *http.Request, key string) string {
	v := strings.TrimSpace(r.Header.Get("Forwarded"))
	if v == "" {
		return ""
	}
	first := strings.TrimSpace(strings.Split(v, ",")[0])
	for _, part := range strings.Split(first, ";") {
		part = strings.TrimSpace(part)
		low := strings.ToLower(part)
		if !strings.HasPrefix(low, key+"=") {
			continue
		}
		val := strings.TrimSpace(part[strings.IndexByte(part, '=')+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		return val
	}
	return ""
}

// forwardedPortFirst returns the first X-Forwarded-Port value, or empty.
func forwardedPortFirst(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Port"))
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw
}

// clientVisiblePort picks the TCP port the browser uses to reach this app (after proxies).
// Used by picoWebUIAddr → buildWsURL / buildPicoEventsURL / buildPicoSendURL so WebSocket and
// HTTP URLs match the dashboard page origin (cookies / token flow behind tunnels and reverse proxies).
func clientVisiblePort(r *http.Request, serverListenPort int) string {
	if p := forwardedPortFirst(r); p != "" {
		return p
	}
	if fwdHost := forwardedHostFirst(r); fwdHost != "" {
		if _, port, err := net.SplitHostPort(fwdHost); err == nil && port != "" {
			return port
		}
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		return port
	}
	if strings.TrimSpace(r.Host) == "" && forwardedHostFirst(r) == "" {
		return strconv.Itoa(serverListenPort)
	}
	if requestHTTPScheme(r) == "https" {
		return "443"
	}
	return "80"
}

// joinClientVisibleHostPort builds host:port for absolute URLs returned to the browser.
func joinClientVisibleHostPort(r *http.Request, host string, serverListenPort int) string {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(h, p)
	}
	return net.JoinHostPort(host, clientVisiblePort(r, serverListenPort))
}

// picoWebUIAddr is host:port for URLs returned to the browser (/pico/ws, /pico/events, /pico/send).
// It must match the HTTP Host the client used (or X-Forwarded-*), not cfg.Gateway.Host — otherwise
// e.g. page on localhost with ws_url 127.0.0.1 omits cookies and the dashboard auth handshake fails.
func (h *Handler) picoWebUIAddr(r *http.Request) string {
	wsPort := h.serverPort
	if wsPort == 0 {
		wsPort = 18800 // default web server port
	}
	if fwdHost := forwardedHostFirst(r); fwdHost != "" {
		return joinClientVisibleHostPort(r, fwdHost, wsPort)
	}
	return joinClientVisibleHostPort(r, requestHostName(r), wsPort)
}

func (h *Handler) buildWsURL(r *http.Request) string {
	return requestWSScheme(r) + "://" + h.picoWebUIAddr(r) + "/pico/ws"
}

func (h *Handler) buildPicoEventsURL(r *http.Request) string {
	return requestHTTPScheme(r) + "://" + h.picoWebUIAddr(r) + "/pico/events"
}

func (h *Handler) buildPicoSendURL(r *http.Request) string {
	return requestHTTPScheme(r) + "://" + h.picoWebUIAddr(r) + "/pico/send"
}
