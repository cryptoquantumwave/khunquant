// KhunQuant Web Console - Web-based chat and management interface
//
// Provides a web UI for chatting with KhunQuant via the Pico Channel WebSocket,
// with configuration management and gateway process control.
//
// Usage:
//
//	go build -o khunquant-web ./web/backend/
//	./khunquant-web [config.json]
//	./khunquant-web -public config.json

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/credential"
	"github.com/cryptoquantumwave/khunquant/web/backend/api"
	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
	"github.com/cryptoquantumwave/khunquant/web/backend/utils"
)

func main() {
	port := flag.String("port", "18800", "Port to listen on")
	public := flag.Bool("public", false, "Listen on all interfaces (0.0.0.0) instead of localhost only")
	noBrowser := flag.Bool("no-browser", false, "Do not auto-open browser on startup")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "KhunQuant Launcher - A web-based configuration editor\n\n")
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [config.json]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Arguments:\n")
		fmt.Fprintf(os.Stderr, "  config.json    Path to the configuration file (default: ~/.khunquant/config.json)\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s                          Use default config path\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s ./config.json             Specify a config file\n", os.Args[0])
		fmt.Fprintf(
			os.Stderr,
			"  %s -public ./config.json     Allow access from other devices on the network\n",
			os.Args[0],
		)
	}
	flag.Parse()

	// Install a file-backed PassphraseProvider so enc:// credentials in config
	// are decrypted automatically when ~/.khunquant/.passphrase exists.
	credential.InstallFileBackedProvider()

	// Resolve config path
	configPath := utils.GetDefaultConfigPath()
	if flag.NArg() > 0 {
		configPath = flag.Arg(0)
	}

	absPath, err := filepath.Abs(configPath)
	if err != nil {
		log.Fatalf("Failed to resolve config path: %v", err)
	}
	err = utils.EnsureOnboarded(absPath)
	if err != nil {
		log.Printf("Warning: Failed to initialize KhunQuant config automatically: %v", err)
	}

	var explicitPort bool
	var explicitPublic bool
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			explicitPort = true
		case "public":
			explicitPublic = true
		}
	})

	launcherPath := launcherconfig.PathForAppConfig(absPath)
	launcherCfg, err := launcherconfig.Load(launcherPath, launcherconfig.Default())
	if err != nil {
		log.Printf("Warning: Failed to load %s: %v", launcherPath, err)
		launcherCfg = launcherconfig.Default()
	}

	effectivePort := *port
	effectivePublic := *public
	if !explicitPort {
		effectivePort = strconv.Itoa(launcherCfg.Port)
	}
	if !explicitPublic {
		effectivePublic = launcherCfg.Public
	}

	portNum, err := strconv.Atoi(effectivePort)
	if err != nil || portNum < 1 || portNum > 65535 {
		if err == nil {
			err = errors.New("must be in range 1-65535")
		}
		log.Fatalf("Invalid port %q: %v", effectivePort, err)
	}

	// Refuse to start in public mode without an explicit CIDR allowlist — the
	// IP allowlist is the only network-level boundary for LAN access.
	if effectivePublic && len(launcherCfg.AllowedCIDRs) == 0 {
		log.Fatalf(
			"Refusing to start in public mode without an IP allowlist.\n" +
				"Add allowed_cidrs to launcher-config.json (e.g. \"192.168.1.0/24\") or\n" +
				"remove the -public flag to restrict access to localhost only.",
		)
	}

	// C-6: with no dashboard password, SessionAuth issues a session to any
	// caller that reaches the launcher over loopback. That is defensible for a
	// single local console, but -public and trusted proxies both put non-local
	// users on the other end: isTrustedLoopbackCaller trusts RemoteAddr, and a
	// same-host reverse proxy or tunnel makes every remote user look like
	// loopback. Warn rather than refuse, so existing deployments keep working.
	if launcherCfg.DashboardPasswordHash == "" &&
		(effectivePublic || len(launcherCfg.TrustedProxyCIDRs) > 0) {
		reason := "trusting proxy headers from trusted_proxy_cidrs"
		if effectivePublic {
			reason = "listening on 0.0.0.0"
			if len(launcherCfg.TrustedProxyCIDRs) > 0 {
				reason = "listening on 0.0.0.0 and trusting proxy headers"
			}
		}
		log.Printf(
			"WARNING: no dashboard password is set (%s).\n"+
				"  Any caller reaching this launcher over loopback - including remote users\n"+
				"  arriving through a reverse proxy or tunnel - gets a session without\n"+
				"  authenticating. Set one with: khunquant launcher set-password",
			reason,
		)
	}

	// Ensure a persistent launcher token exists.
	if launcherCfg.LauncherToken == "" {
		launcherCfg.LauncherToken = generateLauncherToken()
		if saveErr := launcherconfig.Save(launcherPath, launcherCfg); saveErr != nil {
			log.Printf("Warning: failed to persist launcher token: %v", saveErr)
		}
	}

	// Determine listen address
	var addr string
	if effectivePublic {
		addr = "0.0.0.0:" + effectivePort
	} else {
		addr = "127.0.0.1:" + effectivePort
	}

	// Initialize Server components
	mux := http.NewServeMux()

	// API Routes (e.g. /api/status)
	apiHandler := api.NewHandler(absPath)
	apiHandler.SetServerOptions(portNum, effectivePublic, explicitPublic, launcherCfg.AllowedCIDRs)
	apiHandler.SetServerAccessOptions(launcherCfg.AllowLocalhostBypass, launcherCfg.TrustedProxyCIDRs)
	apiHandler.RegisterRoutes(mux)

	// Frontend Embedded Assets
	registerEmbedRoutes(mux)

	// One store shared by the session middleware (which validates and issues),
	// the password middleware (which defers to an established session), and the
	// API handler (which mints on login and revokes on logout).
	middleware.SetSecureCookies(launcherCfg.BehindTLSProxy)
	sessionStore := middleware.NewSessionStore(0)
	apiHandler.SetSessionAuth(sessionStore, launcherCfg.DashboardPasswordHash)

	// SessionAuth is innermost of the three: it validates an existing session
	// cookie, and (only when no dashboard password is configured) auto-issues
	// one for a trusted loopback caller or a valid launcher token. When a
	// password IS configured, PasswordAuth (below) has already gated the
	// request by the time it reaches here, so those auto-issue rules are
	// disabled — see SessionAuth's doc comment for why.
	// H-7: CSRF is enforced for every mutating /api route here rather than
	// relying on each handler to remember. It sits inside the auth chain, so an
	// unauthenticated cross-site request is still rejected as 401 by
	// PasswordAuth before this can answer 403.
	sessionAuthedMux := middleware.SessionAuth(launcherCfg.LauncherToken, sessionStore, launcherCfg.DashboardPasswordHash,
		middleware.CSRFMutations(mux))

	// PasswordAuth wraps SessionAuth: a request must present a valid session,
	// correct Basic credentials, or the launcher token (or hit an exempted
	// path) before SessionAuth's handler — and therefore the mux — ever runs.
	// This ordering is what fixes the dashboard password bypass: previously
	// SessionAuth ran first and could auto-issue a session to any loopback
	// caller with no password check at all.
	passwordProtectedMux := middleware.PasswordAuth(middleware.PasswordAuthConfig{
		Sessions:      sessionStore,
		PasswordHash:  launcherCfg.DashboardPasswordHash,
		LauncherToken: launcherCfg.LauncherToken,
	}, sessionAuthedMux)

	// IP allowlist is outermost of the three: a caller outside the allowed
	// network never reaches the password/session checks at all.
	accessControlledMux, err := middleware.IPAllowlist(middleware.IPAllowlistConfig{
		AllowedCIDRs:         launcherCfg.AllowedCIDRs,
		AllowLocalhostBypass: launcherCfg.AllowLocalhostBypass,
		TrustedProxyCIDRs:    launcherCfg.TrustedProxyCIDRs,
	}, passwordProtectedMux)
	if err != nil {
		log.Fatalf("Invalid allowed CIDR configuration: %v", err)
	}

	// Apply middleware stack
	handler := middleware.Recoverer(
		middleware.Logger(
			middleware.SecurityHeaders(
				middleware.JSONContentType(
					accessControlledMux,
				),
			),
		),
	)

	// Print startup banner
	fmt.Print(utils.Banner)
	fmt.Println()
	fmt.Println("  Open the following URL in your browser:")
	fmt.Println()
	fmt.Printf("    >> http://localhost:%s <<\n", effectivePort)
	if effectivePublic {
		if ip := utils.GetLocalIP(); ip != "" {
			fmt.Printf("    >> http://%s:%s <<\n", ip, effectivePort)
		}
		fmt.Println()
		fmt.Println("  Launcher token is stored in root-only config; see launcher-config.json")
	}
	fmt.Println()

	// Auto-open browser
	if !*noBrowser {
		go func() {
			time.Sleep(500 * time.Millisecond)
			url := "http://localhost:" + effectivePort
			if err := utils.OpenBrowser(url); err != nil {
				log.Printf("Warning: Failed to auto-open browser: %v", err)
			}
		}()
	}

	// Auto-start gateway after backend starts listening.
	go func() {
		time.Sleep(1 * time.Second)
		apiHandler.TryAutoStartGateway()
	}()

	// Start the Server
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

func generateLauncherToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("Failed to generate launcher token: %v", err)
	}
	return hex.EncodeToString(b)
}
