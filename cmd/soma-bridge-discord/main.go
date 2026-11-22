package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ra1ncandy/soma/internal/bridge"
)

func main() {
	var (
		somaDir      string
		botToken     string
		publicKey    string
		channelID    string
		allowedUsers string
		allowedChans string
		listenAddr   string
		pollInterval time.Duration
	)

	flag.StringVar(&somaDir, "soma-dir", envOr("SOMA_DIR", ".soma"), "Path to initialized .soma directory")
	flag.StringVar(&botToken, "token", os.Getenv("DISCORD_BOT_TOKEN"), "Discord Bot Token")
	flag.StringVar(&publicKey, "public-key", os.Getenv("DISCORD_PUBLIC_KEY"), "Discord Interaction Ed25519 public key (hex)")
	flag.StringVar(&channelID, "channel", os.Getenv("DISCORD_CHANNEL_ID"), "Discord channel ID for Inbox alerts")
	flag.StringVar(&allowedUsers, "allowed-users", os.Getenv("DISCORD_ALLOWED_USERS"), "Comma-separated allowlisted Discord user IDs (required fail-closed)")
	flag.StringVar(&allowedChans, "allowed-channels", os.Getenv("DISCORD_ALLOWED_CHANNELS"), "Comma-separated allowlisted Discord channel IDs")
	flag.StringVar(&listenAddr, "listen", envOr("DISCORD_LISTEN_ADDR", "127.0.0.1:8790"), "Interactions HTTP listen address")
	flag.DurationVar(&pollInterval, "poll", 5*time.Second, "Inbox alert poll interval")
	flag.Parse()

	sockPath := filepath.Join(somaDir, "kernel.sock")
	tokPath := filepath.Join(somaDir, "bridge.token")
	tokBytes, err := os.ReadFile(tokPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soma-bridge-discord: read %s failed: %v\n", tokPath, err)
		os.Exit(1)
	}
	client := bridge.NewClient(sockPath, strings.TrimSpace(string(tokBytes)))

	adapter := bridge.NewDiscordAdapter(client, bridge.DiscordConfig{
		BotToken:          botToken,
		PublicKeyHex:      publicKey,
		ChannelID:         channelID,
		AllowedUserIDs:    splitCSV(allowedUsers),
		AllowedChannelIDs: splitCSV(allowedChans),
		ListenAddr:        listenAddr,
		PollInboxInterval: pollInterval,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if h, hErr := client.Health(ctx); hErr != nil {
		fmt.Fprintf(os.Stderr, "soma-bridge-discord: warning: kernel health check failed (%v)\n", hErr)
	} else {
		fmt.Printf("soma-bridge-discord connected to %s (kernel=%s, allowed_users=%d)\n",
			sockPath, h.Version, len(adapter.Config.AllowedUserIDs))
	}

	if err := adapter.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "soma-bridge-discord error: %v\n", err)
		os.Exit(1)
	}
}

func envOr(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
