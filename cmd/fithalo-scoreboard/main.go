// Command fithalo-scoreboard runs a FitHalo Scoreboard.
//
//	fithalo-scoreboard [-data data] [-listen address] [-public-url https://board.example.com]
//	                   [-tls development|files|proxy] [-cert fullchain.pem -key privkey.pem]
//	                   [-trusted-proxies 10.0.0.0/8,::1]
//	fithalo-scoreboard reset-admin [-data data]
//	fithalo-scoreboard check -url https://board.example.com -invite code
//
// It serves TLS 1.2 or later from certificate files, or plain HTTP behind a proxy that terminates TLS,
// or, for development only, plain HTTP on loopback. Started by AWS Lambda, it answers a function URL
// instead, from DynamoDB.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/admin"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/api"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/auth"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/lambda"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/nonces"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/password"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/ratelimit"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/scoreboard"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/serving"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/sharing"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/store"
	"github.com/jacobjohnston/FitHalo-Scoreboards/internal/web"
	"github.com/jacobjohnston/FitHalo-Scoreboards/spec"
)

// version is set at build time with -ldflags "-X main.version=1.0.0".
var version = "development"

const (
	// sweepInterval is the time between sweeps of expired data.
	sweepInterval = time.Hour

	// shutdownTimeout is how long open requests get to finish on shutdown.
	shutdownTimeout = 10 * time.Second

	// apiBurst is the most API requests an address may make at once.
	apiBurst = 120

	// apiPerMinute is the API requests an address may make each minute after its burst.
	apiPerMinute = 300

	// joinBurst is the most attempts to join an address may make at once.
	joinBurst = 5

	// joinPerMinute is the attempts to join an address may make each minute after its burst.
	joinPerMinute = 10

	// signInBurst is the most admin sign-in attempts an address may make at once.
	signInBurst = 10

	// signInPerMinute is the admin sign-in attempts an address may make each minute after its burst.
	signInPerMinute = 10
)

// main runs the subcommand named, the Lambda runtime when started by Lambda, or the server.
func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "reset-admin":
		err = resetAdmin(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "check":
		err = check(os.Args[2:], os.Stdout)
	case os.Getenv("AWS_LAMBDA_RUNTIME_API") != "":
		err = runLambda(logger)
	default:
		err = serve(logger, os.Args[1:])
	}
	if err != nil {
		logger.Error("stopped", "error", err)
		os.Exit(1)
	}
}

// secrets are the values the host keeps in its configuration and never in the store.
type secrets struct {
	// setupCode creates the first admin, or is empty for one generated at start.
	setupCode string

	// recoveryCode recovers the admin once, for a host with no shell, or is empty for none.
	recoveryCode string

	// pepper is mixed into every password hash, or is empty for none.
	pepper []byte
}

// readSecrets reads the secrets from the environment and the pepper file, refusing any too short.
func readSecrets() (secrets, error) {
	result := secrets{setupCode: os.Getenv("SCOREBOARD_SETUP_CODE"), recoveryCode: os.Getenv("SCOREBOARD_RECOVERY_CODE")}
	for name, code := range map[string]string{"SCOREBOARD_SETUP_CODE": result.setupCode, "SCOREBOARD_RECOVERY_CODE": result.recoveryCode} {
		if code != "" && len(code) < admin.MinimumCodeLength {
			return secrets{}, fmt.Errorf("%s must be at least %d characters", name, admin.MinimumCodeLength)
		}
	}
	pepper := os.Getenv("SCOREBOARD_PEPPER")
	if path := os.Getenv("SCOREBOARD_PEPPER_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return secrets{}, fmt.Errorf("reading the pepper: %w", err)
		}
		pepper = strings.TrimSpace(string(data))
	}
	if pepper != "" && len(pepper) < admin.MinimumCodeLength {
		return secrets{}, fmt.Errorf("the pepper must be at least %d characters", admin.MinimumCodeLength)
	}
	result.pepper = []byte(pepper)
	return result, nil
}

// assembled is a board ready to serve: its handler, and the sweeps that keep its store tidy.
type assembled struct {
	// handler serves the API, the landing page and the admin portal.
	handler http.Handler

	// sweep removes expired data from the store.
	sweep func(ctx context.Context) error
}

// assemble builds the board over a store: the API, the landing page and the admin portal, on one
// handler. Every way of running the board goes through here.
func assemble(ctx context.Context, backend store.Store, publicURL *url.URL, keys secrets, dataDirectory string,
	nonceRecorder api.Nonces, clientAddress func(*http.Request) string, logger *slog.Logger) (*assembled, error) {
	allowlist, err := sharing.LoadAllowlist(spec.Sharing)
	if err != nil {
		return nil, err
	}
	service, administrator := newServices(backend, allowlist, keys, dataDirectory, logger)
	initial, err := scoreboard.DefaultBoard()
	if err != nil {
		return nil, err
	}
	if _, _, err := service.EnsureBoard(ctx, initial); err != nil {
		return nil, err
	}
	if err := announceSetup(ctx, administrator, publicURL.String()+"/admin/setup", os.Stderr); err != nil {
		return nil, err
	}

	apiServer := &api.Server{
		Service:       service,
		Nonces:        nonceRecorder,
		Limiter:       ratelimit.New(apiBurst, apiPerMinute),
		JoinLimiter:   ratelimit.New(joinBurst, joinPerMinute),
		Logger:        logger,
		ServerVersion: version,
		ClientAddress: clientAddress,
	}
	site := &web.Site{
		Service:       service,
		Admin:         administrator,
		Logger:        logger,
		PublicURL:     publicURL,
		SignInLimiter: ratelimit.New(signInBurst, signInPerMinute),
		ClientAddress: clientAddress,
	}
	siteHandler, err := site.Handler()
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(publicURL.Path, "/")
	mux := http.NewServeMux()
	mux.Handle(base+"/api/", serving.Compress(apiServer.Handler(base)))
	mux.Handle(base+"/static/", serving.Compress(siteHandler))
	mux.Handle(base+"/", siteHandler)

	storedNonces, _ := nonceRecorder.(*nonces.Store)
	return &assembled{handler: mux, sweep: newSweep(service, administrator, storedNonces)}, nil
}

// announceSetup writes the setup banner while the board has no admin, generating a setup code when none
// is configured. Only a generated code is written, so a configured one never reaches a log.
func announceSetup(ctx context.Context, administrator *admin.Admin, setupURL string, output *os.File) error {
	setUp, err := administrator.IsSetUp(ctx)
	if err != nil || setUp {
		return err
	}
	generated := ""
	if administrator.SetupCode == "" {
		if generated, err = scoreboard.NewIdentifier(); err != nil {
			return err
		}
		administrator.SetupCode = generated
	}
	terminal := isTerminal(output)
	writeSetupBanner(output, setupURL, generated, terminal, terminal && wantsColour())
	return nil
}

// newServices builds the board's service and admin over a store, the same however the board runs.
func newServices(backend store.Store, allowlist *sharing.Allowlist, keys secrets, dataDirectory string,
	logger *slog.Logger) (*scoreboard.Service, *admin.Admin) {
	service := &scoreboard.Service{
		Store: backend, Allowlist: allowlist, Now: time.Now, Pepper: keys.pepper, Logger: logger,
	}
	administrator := &admin.Admin{
		Store: backend, Pepper: keys.pepper, Now: time.Now,
		SetupCode: keys.setupCode, RecoveryCode: keys.recoveryCode, DataDirectory: dataDirectory,
		Breached: func(ctx context.Context, candidate string) (bool, error) {
			breached, err := password.BreachChecker{}.Breached(ctx, candidate)
			if err != nil && logger != nil {
				logger.Warn("could not check a password against Have I Been Pwned; going ahead without it", "error", err)
			}
			return breached, err
		},
	}
	return service, administrator
}

// newSweep returns the sweep that removes expired weekly challenges, sessions, passkey challenges and, when kept,
// nonces.
func newSweep(service *scoreboard.Service, administrator *admin.Admin,
	storedNonces *nonces.Store) func(context.Context) error {
	return func(ctx context.Context) error {
		failures := []error{service.SweepRetention(ctx), administrator.Sweep(ctx)}
		if storedNonces != nil {
			failures = append(failures, storedNonces.Sweep(ctx, time.Now()))
		}
		return errors.Join(failures...)
	}
}

// configuration is what serving on a network reads, from flags and the environment.
type configuration struct {
	// dataDirectory is where a directory store keeps its data.
	dataDirectory string

	// listenAddress is the address and port to listen on.
	listenAddress string

	// publicURL is where members reach the board, with any path it is served under.
	publicURL *url.URL

	// mode is serving.ModeDevelopment, ModeFiles or ModeProxy.
	mode string

	// certificatePath is the certificate chain's file, in files mode.
	certificatePath string

	// keyPath is the private key's file, in files mode.
	keyPath string

	// trustedProxies are the peers whose forwarding headers count in proxy mode, or empty for any peer.
	trustedProxies serving.TrustedProxies

	// trustedProxiesDefaulted is set when proxy mode on every interface fell back to the non-public ranges.
	trustedProxiesDefaulted bool
}

// readConfiguration parses the flags over the environment and refuses a combination that cannot serve.
func readConfiguration(arguments []string) (configuration, error) {
	flags := flag.NewFlagSet("fithalo-scoreboard", flag.ContinueOnError)
	dataDirectory := flags.String("data", environment("SCOREBOARD_DATA", "data"), "the directory the board keeps its data in")
	listenAddress := flags.String("listen", environment("SCOREBOARD_LISTEN", "127.0.0.1:8080"), "the address and port to listen on")
	publicURL := flags.String("public-url", os.Getenv("SCOREBOARD_PUBLIC_URL"), "where members reach the board, with any path it is served under")
	mode := flags.String("tls", environment("SCOREBOARD_TLS", serving.ModeDevelopment), "development, files or proxy")
	certificatePath := flags.String("cert", os.Getenv("SCOREBOARD_CERT"), "the certificate chain, in files mode")
	keyPath := flags.String("key", os.Getenv("SCOREBOARD_KEY"), "the certificate's private key, in files mode")
	trustedProxies := flags.String("trusted-proxies", os.Getenv("SCOREBOARD_TRUSTED_PROXIES"), "the proxies' addresses or CIDRs, or any, in proxy mode")
	if err := flags.Parse(arguments); err != nil {
		return configuration{}, err
	}
	result := configuration{
		dataDirectory: *dataDirectory, listenAddress: *listenAddress, mode: *mode,
		certificatePath: *certificatePath, keyPath: *keyPath,
	}
	if *publicURL == "" {
		_, port, err := net.SplitHostPort(*listenAddress)
		if err != nil {
			return configuration{}, fmt.Errorf("listen address: %w", err)
		}
		*publicURL = "http://localhost:" + port
	}
	parsed, err := parsePublicURL(*publicURL)
	if err != nil {
		return configuration{}, err
	}
	result.publicURL = parsed
	trustAny := strings.TrimSpace(*trustedProxies) == serving.TrustAnyProxy
	if trustAny {
		*trustedProxies = ""
	}
	if result.trustedProxies, err = serving.ParseTrustedProxies(*trustedProxies); err != nil {
		return configuration{}, err
	}
	if (trustAny || len(result.trustedProxies) > 0) && result.mode != serving.ModeProxy {
		return configuration{}, errors.New("SCOREBOARD_TRUSTED_PROXIES (-trusted-proxies) applies only in proxy mode")
	}
	switch result.mode {
	case serving.ModeDevelopment:
		if parsed.Scheme != "http" {
			return configuration{}, errors.New("development mode serves http://localhost; choose -tls files or -tls proxy for an https address")
		}
	case serving.ModeFiles, serving.ModeProxy:
		if parsed.Scheme != "https" {
			return configuration{}, errors.New("the public URL must be https in files and proxy modes")
		}
		if result.mode == serving.ModeFiles && (result.certificatePath == "" || result.keyPath == "") {
			return configuration{}, errors.New("files mode needs -cert and -key")
		}
	default:
		return configuration{}, fmt.Errorf("-tls is development, files or proxy, not %q", result.mode)
	}
	if err := serving.CheckListenAddress(result.mode, result.listenAddress); err != nil {
		return configuration{}, err
	}
	if result.mode == serving.ModeProxy && !trustAny && len(result.trustedProxies) == 0 && listensEverywhere(result.listenAddress) {
		result.trustedProxies, result.trustedProxiesDefaulted = serving.NonPublicProxies(), true
	}
	return result, nil
}

// listensEverywhere reports whether address leaves the host blank or unspecified, so any interface reaches it.
func listensEverywhere(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "" || (ip != nil && ip.IsUnspecified())
}

// parsePublicURL parses the board's address, which must be https unless it is on localhost.
func parsePublicURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSuffix(value, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("the public URL %q is not an http or https address", value)
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return nil, errors.New("the public URL must be https unless it is localhost")
	}
	return parsed, nil
}

// openStore opens the store SCOREBOARD_STORE names: a directory by default, or a cloud database for a
// container that cannot keep a disk of its own.
func openStore(dataDirectory string) (store.Store, error) {
	switch kind := environment("SCOREBOARD_STORE", "directory"); kind {
	case "directory":
		return store.OpenDirectory(dataDirectory)
	case "dynamodb":
		return store.OpenDynamoDB(os.Getenv("SCOREBOARD_DYNAMODB_TABLE"), os.Getenv("AWS_REGION"))
	case "cosmos":
		return store.OpenCosmos(os.Getenv("SCOREBOARD_COSMOS_ENDPOINT"), os.Getenv("SCOREBOARD_COSMOS_KEY"),
			os.Getenv("SCOREBOARD_COSMOS_DATABASE"), os.Getenv("SCOREBOARD_COSMOS_CONTAINER"))
	default:
		return nil, fmt.Errorf("SCOREBOARD_STORE is directory, dynamodb or cosmos, not %q", kind)
	}
}

// serve runs the board on a network until interrupted or terminated.
func serve(logger *slog.Logger, arguments []string) error {
	settings, err := readConfiguration(arguments)
	if err != nil {
		return err
	}
	keys, err := readSecrets()
	if err != nil {
		return err
	}
	opened, err := openStore(settings.dataDirectory)
	if err != nil {
		return err
	}
	backend := store.Compress(opened)
	defer backend.Close()

	var nonceRecorder api.Nonces = auth.NewNonceCache()
	if _, isDirectory := opened.(*store.Directory); !isDirectory {
		nonceRecorder = &nonces.Store{Store: backend, Logger: logger}
	}
	var clientAddress func(*http.Request) string
	if settings.mode == serving.ModeProxy {
		clientAddress = settings.trustedProxies.ClientAddress
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	board, err := assemble(ctx, backend, settings.publicURL, keys, settings.dataDirectory, nonceRecorder, clientAddress, logger)
	if err != nil {
		return err
	}

	handler := board.handler
	if settings.mode == serving.ModeProxy {
		handler = settings.trustedProxies.Require(serving.RequireProxyHTTPS(handler))
	}
	httpServer := &http.Server{
		Addr:              settings.listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	go sweepHourly(ctx, board.sweep, logger)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		httpServer.Shutdown(shutdown)
	}()

	if settings.trustedProxiesDefaulted {
		logger.Info("trusting forwarding headers from loopback and private networks only; set SCOREBOARD_TRUSTED_PROXIES to change")
	}
	logger.Info("serving", "address", settings.listenAddress, "publicURL", settings.publicURL.String(), "mode", settings.mode,
		"trustedProxies", len(settings.trustedProxies), "version", version)
	if settings.mode == serving.ModeFiles {
		reloader, err := serving.NewCertificateReloader(settings.certificatePath, settings.keyPath)
		if err != nil {
			return err
		}
		httpServer.TLSConfig = serving.TLSConfig(reloader)
		if err := httpServer.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runLambda answers a function URL from DynamoDB. The board is assembled on the first request, since
// the function URL's address, which invites and passkeys need, is only known once one arrives.
func runLambda(logger *slog.Logger) error {
	keys, err := readSecrets()
	if err != nil {
		return err
	}
	table, err := store.OpenDynamoDB(os.Getenv("SCOREBOARD_DYNAMODB_TABLE"), os.Getenv("AWS_REGION"))
	if err != nil {
		return err
	}
	backend := store.Compress(table)
	ctx := context.Background()
	recorder := &nonces.Store{Store: backend, Logger: logger}
	runtime := &lambda.Runtime{
		API:    os.Getenv("AWS_LAMBDA_RUNTIME_API"),
		Logger: logger,
		Handler: func(domain string) (http.Handler, error) {
			address := os.Getenv("SCOREBOARD_PUBLIC_URL")
			if address == "" {
				address = "https://" + domain
			}
			publicURL, err := parsePublicURL(address)
			if err != nil {
				return nil, err
			}
			board, err := assemble(ctx, backend, publicURL, keys, "", recorder, nil, logger)
			if err != nil {
				return nil, err
			}
			return board.handler, nil
		},
		Scheduled: func(ctx context.Context) error {
			allowlist, err := sharing.LoadAllowlist(spec.Sharing)
			if err != nil {
				return err
			}
			service, administrator := newServices(backend, allowlist, keys, "", logger)
			return newSweep(service, administrator, recorder)(ctx)
		},
	}
	logger.Info("answering Lambda", "version", version)
	return runtime.Run(ctx)
}

// resetAdmin leaves a recovery code for the running server and prints it, for a host with a shell.
func resetAdmin(arguments []string) error {
	flags := flag.NewFlagSet("reset-admin", flag.ContinueOnError)
	dataDirectory := flags.String("data", environment("SCOREBOARD_DATA", "data"), "the board's data directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if _, err := os.Stat(*dataDirectory); err != nil {
		return fmt.Errorf("the data directory: %w", err)
	}
	code, err := admin.WriteRecoveryFile(*dataDirectory, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("Recovery code: %s\nEnter it at /admin/recover within %d minutes. It works once.\n", code, int(admin.RecoveryLifetime.Minutes()))
	return nil
}

// environment returns the environment variable name, or fallback when it is unset or empty.
func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// sweepHourly runs sweep at once and then every sweepInterval until ctx ends, logging failures.
func sweepHourly(ctx context.Context, sweep func(context.Context) error, logger *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		if err := sweep(ctx); err != nil && ctx.Err() == nil {
			logger.Error("sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
