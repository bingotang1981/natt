// Command natt is the unified NAT traversal tool.
//
// Usage:
//
//	natt server -c server.json     # start the server
//	natt client -c client.json     # start the client
//	natt keygen                    # generate an AES-256 key
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"natt/pkg/client"
	"natt/pkg/config"
	"natt/pkg/crypto"
	"natt/pkg/server"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	sub := os.Args[1]
	switch sub {
	case "server":
		runServer(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	case "keygen":
		runKeygen()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage:
  natt server -c <config>       start the server
  natt client -c <config>       start the client
  natt keygen                   generate an AES-256 key

Run 'natt server -h' or 'natt client -h' for subcommand-specific flags.`)
}

// ---------------------------------------------------------------------------
// Server subcommand
// ---------------------------------------------------------------------------

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	configPath := fs.String("c", "server.json", "config file path")
	_ = fs.Parse(args)

	// Handle keygen when called as "natt server keygen" (backward compat)
	if fs.Arg(0) == "keygen" {
		runKeygen()
		return
	}

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	initLogger(cfg.LogLevel)

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create server: %v\n", err)
		os.Exit(1)
	}

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("shutting down gracefully...")
		srv.Stop()
	}()

	if err := srv.Start(); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	srv.Wait()
	slog.Info("server stopped")
}

// ---------------------------------------------------------------------------
// Client subcommand
// ---------------------------------------------------------------------------

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	configPath := fs.String("c", "client.json", "config file path")
	serverAddr := fs.String("s", "", "server address (overrides config)")
	serverPort := fs.Int("p", 0, "server port (overrides config)")
	_ = fs.Parse(args)

	cfg, err := config.LoadClientConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	// CLI overrides
	if *serverAddr != "" {
		cfg.ServerAddr = *serverAddr
	}
	if *serverPort > 0 {
		cfg.ServerPort = *serverPort
	}

	initLogger(cfg.LogLevel)

	cli, err := client.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create client: %v\n", err)
		os.Exit(1)
	}

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("shutting down...")
		cli.Stop()
	}()

	slog.Info("client starting",
		"server", net.JoinHostPort(cfg.ServerAddr, strconv.Itoa(cfg.ServerPort)),
		"proxies", len(cfg.Proxies),
	)

	if err := cli.Run(); err != nil {
		slog.Error("client error", "error", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Keygen subcommand
// ---------------------------------------------------------------------------

func runKeygen() {
	key, err := crypto.GenerateKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to generate key: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(key)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func initLogger(level string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(level),
	})))
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
