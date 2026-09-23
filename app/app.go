// Package app wires a statically selected set of adapters into the gateway.
// A private build can import this package from its own main package.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/gateway"
	"github.com/denta-codex/codex-gateway/internal/catalog"
	"github.com/denta-codex/codex-gateway/internal/subscription"
	"github.com/denta-codex/codex-gateway/internal/verify"
)

func Run(ctx context.Context, args []string, extensions ...adapter.Adapter) error {
	if len(args) == 0 {
		return errors.New("usage: codex-gateway serve|catalog refresh|verify-model-list")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	defaults := struct{ auth, catalog string }{filepath.Join(home, ".codex/auth.json"), filepath.Join(home, ".local/state/codex-gateway/model-catalog.json")}
	flags := flag.NewFlagSet("codex-gateway", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:48766", "gateway listener")
	authPath := flags.String("auth-file", defaults.auth, "Grace Codex auth.json")
	catalogPath := flags.String("catalog", defaults.catalog, "merged Codex model catalog")
	codexBinary := flags.String("codex-binary", "codex", "installed Codex CLI")
	upstream := flags.String("upstream", "https://chatgpt.com/backend-api/codex", "fixed subscription upstream")
	noAuthRefresh := flags.Bool("no-auth-refresh", false, "read credential without rotation (catalog preview only)")
	commandArgs := args[1:]
	if args[0] == "catalog" {
		if len(commandArgs) == 0 || commandArgs[0] != "refresh" {
			return errors.New("usage: codex-gateway catalog refresh [flags]")
		}
		commandArgs = commandArgs[1:]
	}
	if err := flags.Parse(commandArgs); err != nil {
		return err
	}
	auth := &subscription.Auth{Path: *authPath, CodexBinary: *codexBinary}
	switch args[0] {
	case "catalog":
		if len(flags.Args()) != 0 {
			return errors.New("unexpected catalog arguments")
		}
		var source catalog.TokenSource = auth
		if *noAuthRefresh {
			source = readOnlyAuth{auth}
		}
		return catalog.Refresh(ctx, *catalogPath, catalog.SubscriptionSource(source, &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, *upstream, *codexBinary), extensions)
	case "verify-model-list":
		if len(flags.Args()) != 0 {
			return errors.New("unexpected verify arguments")
		}
		return verify.ModelList(ctx, *codexBinary, *catalogPath, *upstream)
	case "serve":
		if len(flags.Args()) != 0 {
			return errors.New("unexpected serve arguments")
		}
		host, _, err := net.SplitHostPort(*listen)
		if err != nil || host != "127.0.0.1" {
			return errors.New("v0 gateway must listen on 127.0.0.1")
		}
		g, err := gateway.New(gateway.Config{UpstreamBase: *upstream, CatalogPath: *catalogPath, Auth: auth, Logger: log.Default()}, extensions...)
		if err != nil {
			return err
		}
		server := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 15 * time.Second}
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
		log.Printf("gateway listening on %s", *listen)
		err = server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve gateway: %w", err)
	default:
		return errors.New("usage: codex-gateway serve|catalog refresh|verify-model-list")
	}
}

type readOnlyAuth struct{ *subscription.Auth }

func (a readOnlyAuth) Token(ctx context.Context) (string, string, error) { return a.ReadOnlyToken(ctx) }
