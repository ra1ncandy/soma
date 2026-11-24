package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/bridge"
	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/inbox"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/tui"
	"github.com/ra1ncandy/soma/internal/webui"
)

func newUICmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Launch the 4-tab Bubble Tea v2 operator workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			_, err = tea.NewProgram(tui.New(st, absDir)).Run()
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	return cmd
}

func newDaemonCmd() *cobra.Command {
	dCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run and inspect the Tier-0 zero-token event daemon, coalescer & brakes",
	}

	var startDir, cfgPath string
	var withBridge, useMock bool
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start Tier-0 event daemon (webhooks, sentinels, fsnotify, TTL GC)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(startDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			cfg, err := loadDaemonConfig(st, cfgPath)
			if err != nil {
				return err
			}
			prov, modelID, pErr := selectOptionalProvider(absDir, useMock)
			if pErr == nil && modelID != "" {
				_ = ensureDefaultAccount(cmd.Context(), st, modelID)
			}

			d := daemon.New(st, cfg, absDir)
			d.Provider = prov

			runCtx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			var wg sync.WaitGroup
			errCh := make(chan error, 2)

			if withBridge {
				ks, bErr := bridge.NewKernelServer(st, bridge.ServerConfig{
					SomaDir:      st.SomaDir(),
					WorkspaceDir: absDir,
					Version:      Version,
					Provider:     prov,
				})
				if bErr != nil {
					return bErr
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Bridge listening: %s\n", ks.SocketPath())
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := ks.Start(runCtx); err != nil {
						errCh <- err
					}
				}()
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Tier-0 daemon started (webhook=%s, triggers=%d)\n",
				cfg.WebhookAddr, len(cfg.Triggers))
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := d.Start(runCtx); err != nil {
					errCh <- err
				}
			}()

			wg.Wait()
			close(errCh)
			for e := range errCh {
				if e != nil {
					return e
				}
			}
			return nil
		},
	}
	startCmd.Flags().StringVar(&startDir, "dir", ".", "Workspace directory")
	startCmd.Flags().StringVar(&cfgPath, "config", "", "Path to triggers.yaml (default .soma/triggers.yaml)")
	startCmd.Flags().BoolVar(&withBridge, "bridge", false, "Also start Unix socket bridge server")
	startCmd.Flags().BoolVar(&useMock, "mock", false, "Use mock provider for trigger execution")

	var statusDir string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Inspect Tier-0 daemon trigger_runs, inhibitory brakes, and pending inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(statusDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			ctx := cmd.Context()
			trigs, err := st.ListTriggerRuns(ctx, "", 100)
			if err != nil {
				return err
			}
			pendingInbox, err := st.CountPendingInbox(ctx)
			if err != nil {
				return err
			}
			accs, err := st.ListAccounts(ctx)
			if err != nil {
				return err
			}
			var skipped, coalesced, done, failed int
			for _, tr := range trigs {
				switch tr.Status {
				case "skipped":
					skipped++
				case "coalesced":
					coalesced++
				case "done":
					done++
				case "failed":
					failed++
				}
			}
			nowMs := time.Now().UnixMilli()
			cooling := 0
			for _, a := range accs {
				if a.CooldownUntil > nowMs {
					cooling++
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Tier-0 Daemon Status (%s):\n", absDir)
			fmt.Fprintf(out, "  Trigger runs: total=%d | zero_token_skipped=%d | coalesced=%d | done=%d | failed=%d\n",
				len(trigs), skipped, coalesced, done, failed)
			fmt.Fprintf(out, "  Inbox pending: %d | Accounts: %d total (%d cooling down)\n",
				pendingInbox, len(accs), cooling)
			return nil
		},
	}
	statusCmd.Flags().StringVar(&statusDir, "dir", ".", "Workspace directory")

	var dryDir, dryCfg string
	dryCmd := &cobra.Command{
		Use:   "dry-run <trigger_id>",
		Short: "Evaluate inhibitory brakes and sentinel command without dispatching",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dryDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			cfg, err := loadDaemonConfig(st, dryCfg)
			if err != nil {
				return err
			}
			d := daemon.New(st, cfg, absDir)
			rep, err := d.DryRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	dryCmd.Flags().StringVar(&dryDir, "dir", ".", "Workspace directory")
	dryCmd.Flags().StringVar(&dryCfg, "config", "", "Path to triggers.yaml (default .soma/triggers.yaml)")

	dCmd.AddCommand(startCmd, statusCmd, dryCmd)
	return dCmd
}

func loadDaemonConfig(st *store.Store, cfgPath string) (daemon.Config, error) {
	explicit := strings.TrimSpace(cfgPath) != ""
	if !explicit {
		cfgPath = filepath.Join(st.SomaDir(), "triggers.yaml")
	}
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return daemon.DefaultConfig(), nil
		}
		return daemon.Config{}, err
	}
	return cfg, nil
}

func selectOptionalProvider(workspaceDir string, useMock bool) (dag.ModelProvider, string, error) {
	if useMock || strings.TrimSpace(os.Getenv("SOMA_API_BASE")) != "" {
		return selectProvider(workspaceDir, useMock)
	}
	if st, sErr := openStore(workspaceDir); sErr == nil {
		defer func() { _ = st.Close() }()
		if prov, m, ok := webui.ResolveDefaultProviderFromHarness(context.Background(), st, workspaceDir); ok {
			return prov, m, nil
		}
	} else if prov, m, ok := webui.ResolveDefaultProviderFromHarness(context.Background(), nil, workspaceDir); ok {
		return prov, m, nil
	}
	return nil, "", nil
}

func newBridgeCmd() *cobra.Command {
	bCmd := &cobra.Command{
		Use:   "bridge",
		Short: "Serve kernel Unix IPC socket and start external Channel Bridges",
	}

	var serveDir string
	var serveMock bool
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start authenticated Unix Domain Socket kernel bridge server (.soma/kernel.sock)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(serveDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			prov, modelID, _ := selectOptionalProvider(absDir, serveMock)
			if modelID != "" {
				_ = ensureDefaultAccount(cmd.Context(), st, modelID)
			}

			ks, err := bridge.NewKernelServer(st, bridge.ServerConfig{
				SomaDir:      st.SomaDir(),
				WorkspaceDir: absDir,
				Version:      Version,
				Provider:     prov,
			})
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			fmt.Fprintf(cmd.OutOrStdout(), "soma bridge serving unix socket %s (token=%s)\n",
				ks.SocketPath(), ks.TokenPath())
			return ks.Start(ctx)
		},
	}
	serveCmd.Flags().StringVar(&serveDir, "dir", ".", "Workspace directory")
	serveCmd.Flags().BoolVar(&serveMock, "mock", false, "Attach mock ModelProvider for remote /run commands")

	var listDir string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List supported Channel Bridge adapters and kernel socket readiness",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(listDir)
			if err != nil {
				return err
			}
			somaDir := filepath.Join(absDir, ".soma")
			sockPath := filepath.Join(somaDir, "kernel.sock")
			tokPath := filepath.Join(somaDir, "bridge.token")

			sockState := "not running"
			if _, sErr := os.Stat(sockPath); sErr == nil {
				sockState = "socket present"
				if tokBytes, tErr := os.ReadFile(tokPath); tErr == nil {
					if h, hErr := bridge.NewClient(sockPath, strings.TrimSpace(string(tokBytes))).Health(cmd.Context()); hErr == nil && h.OK {
						sockState = fmt.Sprintf("READY (kernel=%s)", h.Version)
					}
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Channel Bridge Adapters (%s):\n", absDir)
			fmt.Fprintf(out, "  Kernel socket: %s [%s]\n", sockPath, sockState)
			fmt.Fprintf(out, "  - discord      (Ed25519 interactions + bot alerts)\n")
			fmt.Fprintf(out, "  - telegram     (Bot API long-poll updates + inbox alerts)\n")
			fmt.Fprintf(out, "  - slack        (Events API webhook + chat.postMessage)\n")
			fmt.Fprintf(out, "  - matrix       (Client-Server v3 /sync + room messaging)\n")
			fmt.Fprintf(out, "  - mattermost   (Bot API v4 + outgoing webhook)\n")
			fmt.Fprintf(out, "  - teams        (Incoming Webhook MessageCard)\n")
			fmt.Fprintf(out, "  - google-chat  (Event webhook + synchronous replies)\n")
			fmt.Fprintf(out, "  - email        (SMTP outbound notifications)\n")
			fmt.Fprintf(out, "  - whatsapp     (Sidecar JSON webhook bridge)\n")
			fmt.Fprintf(out, "  - signal       (Sidecar JSON webhook bridge)\n")
			fmt.Fprintf(out, "  - sms          (Sidecar JSON webhook bridge)\n")
			return nil
		},
	}
	listCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")

	var startDir, listenAddr string
	startCmd := &cobra.Command{
		Use:   "start <adapter>",
		Short: "Start an external Channel Bridge (discord|telegram|slack|matrix|mattermost|google-chat|whatsapp|signal|sms) connected to .soma/kernel.sock",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(startDir)
			if err != nil {
				return err
			}
			somaDir := filepath.Join(absDir, ".soma")
			tokBytes, err := os.ReadFile(filepath.Join(somaDir, "bridge.token"))
			if err != nil {
				return fmt.Errorf("read bridge.token (start `soma bridge serve` first): %w", err)
			}
			client := bridge.NewClient(filepath.Join(somaDir, "kernel.sock"), strings.TrimSpace(string(tokBytes)))
			hCfg := webui.LoadWorkspaceHarnessConfig(absDir)
			brCfg := hCfg.Bridges
			ps := bridge.NewPairingStore(somaDir)
			envOr := func(k, fallback string) string {
				if v := strings.TrimSpace(os.Getenv(k)); v != "" {
					return v
				}
				return fallback
			}
			listOr := func(envKey string, fallback []string) []string {
				if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
					return splitCommaList(v)
				}
				return fallback
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			switch strings.ToLower(args[0]) {
			case "discord":
				adp := bridge.NewDiscordAdapter(client, bridge.DiscordConfig{
					BotToken:          envOr("DISCORD_BOT_TOKEN", brCfg.Discord.BotToken),
					PublicKeyHex:      envOr("DISCORD_PUBLIC_KEY", brCfg.Discord.PublicKeyHex),
					ChannelID:         envOr("DISCORD_CHANNEL_ID", brCfg.Discord.ChannelID),
					AllowedUserIDs:    listOr("DISCORD_ALLOWED_USERS", brCfg.Discord.AllowedUserIDs),
					AllowedChannelIDs: listOr("DISCORD_ALLOWED_CHANNELS", brCfg.Discord.AllowedChannelIDs),
					ListenAddr:        listenAddr,
				})
				adp.Pairing = ps
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Discord bridge (listen=%s, allowed_users=%d)\n",
					listenAddr, len(adp.Config.AllowedUserIDs))
				return adp.Run(ctx)

			case "telegram":
				alertChat := brCfg.Telegram.AlertChatID
				if v := strings.TrimSpace(os.Getenv("TELEGRAM_ALERT_CHAT_ID")); v != "" {
					alertChat, _ = strconv.ParseInt(v, 10, 64)
				}
				allowedTGUsers := brCfg.Telegram.AllowedUserIDs
				if v := strings.TrimSpace(os.Getenv("TELEGRAM_ALLOWED_USERS")); v != "" {
					allowedTGUsers = splitCommaInt64(v)
				}
				allowedTGChats := brCfg.Telegram.AllowedChatIDs
				if v := strings.TrimSpace(os.Getenv("TELEGRAM_ALLOWED_CHATS")); v != "" {
					allowedTGChats = splitCommaInt64(v)
				}
				adp := bridge.NewTelegramAdapter(client, bridge.TelegramConfig{
					BotToken:       envOr("TELEGRAM_BOT_TOKEN", brCfg.Telegram.BotToken),
					AllowedUserIDs: allowedTGUsers,
					AllowedChatIDs: allowedTGChats,
					AlertChatID:    alertChat,
				})
				adp.Pairing = ps
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Telegram bridge (allowed_users=%d)\n",
					len(adp.Config.AllowedUserIDs))
				return adp.Run(ctx)

			case "slack":
				gw := bridge.NewGateway(client, somaDir)
				adp := bridge.NewSlackAdapter(gw, bridge.SlackConfig{
					BotToken:          envOr("SLACK_BOT_TOKEN", brCfg.Slack.BotToken),
					SigningSecret:     envOr("SLACK_SIGNING_SECRET", brCfg.Slack.SigningSecret),
					AllowedUserIDs:    listOr("SLACK_ALLOWED_USERS", brCfg.Slack.AllowedUserIDs),
					AllowedChannelIDs: listOr("SLACK_ALLOWED_CHANNELS", brCfg.Slack.AllowedChannelIDs),
				})
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Slack bridge on http://%s\n", listenAddr)
				return serveHTTPBridge(ctx, listenAddr, adp.Handler())

			case "matrix":
				gw := bridge.NewGateway(client, somaDir)
				adp := bridge.NewMatrixAdapter(gw, bridge.MatrixConfig{
					HomeserverURL:  envOr("MATRIX_HOMESERVER_URL", brCfg.Matrix.HomeserverURL),
					AccessToken:    envOr("MATRIX_ACCESS_TOKEN", brCfg.Matrix.AccessToken),
					UserID:         envOr("MATRIX_USER_ID", brCfg.Matrix.UserID),
					AllowedSenders: listOr("MATRIX_ALLOWED_SENDERS", brCfg.Matrix.AllowedSenders),
					AllowedRooms:   listOr("MATRIX_ALLOWED_ROOMS", brCfg.Matrix.AllowedRooms),
				})
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Matrix bridge (%s)\n", adp.Config.HomeserverURL)
				return adp.Run(ctx)

			case "mattermost":
				gw := bridge.NewGateway(client, somaDir)
				adp := bridge.NewMattermostAdapter(gw, bridge.MattermostConfig{
					ServerURL:      envOr("MATTERMOST_URL", brCfg.Mattermost.ServerURL),
					BotToken:       envOr("MATTERMOST_BOT_TOKEN", brCfg.Mattermost.BotToken),
					WebhookURL:     envOr("MATTERMOST_WEBHOOK_URL", brCfg.Mattermost.WebhookURL),
					OutgoingToken:  envOr("MATTERMOST_OUTGOING_TOKEN", brCfg.Mattermost.OutgoingToken),
					AllowedUserIDs: listOr("MATTERMOST_ALLOWED_USERS", brCfg.Mattermost.AllowedUserIDs),
				})
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Mattermost bridge on http://%s\n", listenAddr)
				return serveHTTPBridge(ctx, listenAddr, adp.Handler())

			case "google-chat":
				gw := bridge.NewGateway(client, somaDir)
				adp := bridge.NewGoogleChatAdapter(gw, envOr("GOOGLE_CHAT_WEBHOOK_URL", brCfg.GoogleChat.WebhookURL), listOr("GOOGLE_CHAT_ALLOWED_USERS", brCfg.GoogleChat.AllowedUserIDs))
				fmt.Fprintf(cmd.OutOrStdout(), "Starting Google Chat bridge on http://%s\n", listenAddr)
				return serveHTTPBridge(ctx, listenAddr, adp.Handler())

			case "whatsapp", "signal", "sms":
				gw := bridge.NewGateway(client, somaDir)
				ch := strings.ToLower(args[0])
				prefix := strings.ToUpper(ch)
				var sc webui.SidecarBridgeCfg
				switch ch {
				case "whatsapp":
					sc = brCfg.WhatsApp
				case "signal":
					sc = brCfg.Signal
				case "sms":
					sc = brCfg.SMS
				}
				adp := bridge.NewWebhookBridgeAdapter(gw, bridge.WebhookBridgeConfig{
					ChannelName:    ch,
					OutboundURL:    envOr(prefix+"_OUTBOUND_URL", sc.OutboundURL),
					BearerToken:    envOr(prefix+"_BEARER_TOKEN", sc.BearerToken),
					HMACSecret:     envOr(prefix+"_HMAC_SECRET", sc.HMACSecret),
					AllowedSenders: listOr(prefix+"_ALLOWED_SENDERS", sc.AllowedSenders),
				})
				fmt.Fprintf(cmd.OutOrStdout(), "Starting %s webhook bridge on http://%s\n", ch, listenAddr)
				return serveHTTPBridge(ctx, listenAddr, adp.Handler())

			default:
				return fmt.Errorf("unsupported bridge adapter %q", args[0])
			}
		},
	}
	startCmd.Flags().StringVar(&startDir, "dir", ".", "Workspace directory")
	startCmd.Flags().StringVar(&listenAddr, "listen", "127.0.0.1:8790", "Discord Interactions HTTP listen address")

	var pairDir string
	pairCmd := &cobra.Command{
		Use:   "pair <code>",
		Short: "Approve a pending DM pairing code for a messaging channel sender",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(pairDir)
			if err != nil {
				return err
			}
			ps := bridge.NewPairingStore(filepath.Join(absDir, ".soma"))
			paired, err := ps.ApproveCode(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Approved %s sender %s (%s)\n", paired.Channel, paired.SenderID, paired.Name)
			return nil
		},
	}
	pairCmd.Flags().StringVar(&pairDir, "dir", ".", "Workspace directory")

	var pairedDir string
	pairedCmd := &cobra.Command{
		Use:   "paired",
		Short: "List approved paired senders and pending DM pairing codes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(pairedDir)
			if err != nil {
				return err
			}
			ps := bridge.NewPairingStore(filepath.Join(absDir, ".soma"))
			approved, err := ps.ListApproved()
			if err != nil {
				return err
			}
			pending, err := ps.ListPending()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Approved (%d):\n", len(approved))
			for _, a := range approved {
				fmt.Fprintf(cmd.OutOrStdout(), "  - %-10s %s (%s)\n", a.Channel, a.SenderID, a.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pending (%d):\n", len(pending))
			for _, p := range pending {
				fmt.Fprintf(cmd.OutOrStdout(), "  - %-10s %-10s %s (%s)\n", p.Code, p.Channel, p.SenderID, p.Name)
			}
			return nil
		},
	}
	pairedCmd.Flags().StringVar(&pairedDir, "dir", ".", "Workspace directory")

	var unpairDir string
	unpairCmd := &cobra.Command{
		Use:   "unpair <channel> <sender_id>",
		Short: "Revoke a paired sender's access",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(unpairDir)
			if err != nil {
				return err
			}
			ps := bridge.NewPairingStore(filepath.Join(absDir, ".soma"))
			ok, err := ps.Revoke(args[0], args[1])
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("sender %s on %s is not paired", args[1], args[0])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s sender %s\n", args[0], args[1])
			return nil
		},
	}
	unpairCmd.Flags().StringVar(&unpairDir, "dir", ".", "Workspace directory")

	bCmd.AddCommand(serveCmd, listCmd, startCmd, pairCmd, pairedCmd, unpairCmd)
	return bCmd
}

func newAccountCmd() *cobra.Command {
	accCmd := &cobra.Command{
		Use:   "account",
		Short: "Inspect and manage subscription and metered account leases & cooldowns",
	}

	var listDir string
	runList := func(cmd *cobra.Command, _ []string) error {
		absDir, err := filepath.Abs(listDir)
		if err != nil {
			return err
		}
		st, err := openStore(absDir)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()

		accounts, err := st.ListAccounts(cmd.Context())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Accounts (%d):\n", len(accounts))
		nowMs := time.Now().UnixMilli()
		for _, a := range accounts {
			cool := "ready"
			if a.CooldownUntil > nowMs {
				cool = fmt.Sprintf("cooldown(%ds)", (a.CooldownUntil-nowMs)/1000)
			}
			fmt.Fprintf(out, "  - %s provider=%s model=%s mode=%s leases=%d/%d spend=$%.2f/$%.2f state=%s\n",
				a.ID, a.Provider, a.ModelID, a.BillingMode, a.ActiveLeases, a.MaxConcurrent, a.DailySpentUSD, a.DailyBudgetUSD, cool)
		}
		return nil
	}

	listCmd := &cobra.Command{Use: "list", Short: "List accounts, leases, spend, and cooldowns", RunE: runList}
	listCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")

	statusCmd := &cobra.Command{Use: "status", Short: "Show account lease and cooldown status", RunE: runList}
	statusCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")

	var addDir, id, provider, model, mode string
	var maxConc int
	var dailyBudget float64
	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Register or update a subscription, metered, or local account",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(model) == "" {
				return errors.New("--id and --model are required")
			}
			if mode != "subscription" && mode != "metered" && mode != "local" {
				return errors.New("--mode must be subscription, metered, or local")
			}
			absDir, err := filepath.Abs(addDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.UpsertAccount(cmd.Context(), store.Account{
				ID:             id,
				Provider:       provider,
				ModelID:        model,
				BillingMode:    mode,
				MaxConcurrent:  maxConc,
				DailyBudgetUSD: dailyBudget,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved account %s (provider=%s model=%s mode=%s max_concurrent=%d)\n",
				id, provider, model, mode, maxConc)
			return nil
		},
	}
	addCmd.Flags().StringVar(&addDir, "dir", ".", "Workspace directory")
	addCmd.Flags().StringVar(&id, "id", "", "Account identifier")
	addCmd.Flags().StringVar(&provider, "provider", "openai-codex", "Provider name")
	addCmd.Flags().StringVar(&model, "model", "", "Model ID")
	addCmd.Flags().StringVar(&mode, "mode", "subscription", "Billing mode (subscription|metered|local)")
	addCmd.Flags().IntVar(&maxConc, "max-concurrent", 1, "Maximum concurrent leases")
	addCmd.Flags().Float64Var(&dailyBudget, "daily-budget", 0, "Daily spend ceiling in USD (metered accounts)")

	var resetDir string
	resetCmd := &cobra.Command{
		Use:   "reset-cooldown <account_id>",
		Short: "Clear active rate-limit cooldown on account_id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(resetDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.ResetCooldown(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reset cooldown for account %s\n", args[0])
			return nil
		},
	}
	resetCmd.Flags().StringVar(&resetDir, "dir", ".", "Workspace directory")

	accCmd.AddCommand(listCmd, statusCmd, addCmd, resetCmd)
	return accCmd
}

func newInboxCmd() *cobra.Command {
	inCmd := &cobra.Command{
		Use:   "inbox",
		Short: "List, approve, or dismiss operator Inbox items",
	}

	var listDir, listStatus string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List operator inbox items",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(listDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			filter := listStatus
			if filter == "all" {
				filter = ""
			}
			items, err := st.ListInboxItems(cmd.Context(), filter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Inbox items (%d):\n", len(items))
			for _, it := range items {
				fmt.Fprintf(out, "  [%s] status=%s kind=%s urgency=%s title=%q ref=%s\n",
					it.ID, it.Status, it.ItemKind, it.Urgency, it.Title, it.ArtifactRef)
			}
			return nil
		},
	}
	listCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")
	listCmd.Flags().StringVar(&listStatus, "status", "pending", "Filter by status (pending|approved|dismissed|all)")

	var appDir string
	approveCmd := &cobra.Command{
		Use:   "approve <item_id>",
		Short: "Approve an inbox item via the shared Inbox workflow",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(appDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			msg, err := inbox.NewService(st).Approve(cmd.Context(), args[0], "cli")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
	approveCmd.Flags().StringVar(&appDir, "dir", ".", "Workspace directory")

	var disDir string
	dismissCmd := &cobra.Command{
		Use:   "dismiss <item_id>",
		Short: "Dismiss an inbox item safely",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(disDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			msg, err := inbox.NewService(st).Dismiss(cmd.Context(), args[0], "cli")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
	dismissCmd.Flags().StringVar(&disDir, "dir", ".", "Workspace directory")

	inCmd.AddCommand(listCmd, approveCmd, dismissCmd)
	return inCmd
}

func newConsolidateCmd() *cobra.Command {
	var dir, scope string
	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Audit active memory for dead paths/duplicates and stage a consolidation report in Inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			ctx := cmd.Context()
			svc := memory.NewService(st)
			stale, err := svc.AuditWorkspace(ctx, scope, absDir)
			if err != nil {
				return err
			}
			nodes, err := st.ListActiveMemory(ctx, scope, nil)
			if err != nil {
				return err
			}

			normGroups := make(map[string][]string)
			for _, n := range nodes {
				key := strings.ToLower(strings.Join(strings.Fields(n.Content), " "))
				normGroups[key] = append(normGroups[key], n.ID)
			}
			var dupGroups [][]string
			for _, ids := range normGroups {
				if len(ids) > 1 {
					dupGroups = append(dupGroups, ids)
				}
			}

			var sum strings.Builder
			fmt.Fprintf(&sum, "Audited %d active memory node(s): %d stale file reference(s), %d duplicate group(s).\n",
				len(nodes), len(stale), len(dupGroups))
			for _, f := range stale {
				fmt.Fprintf(&sum, "  stale [%s] missing path %q: %s\n", f.NodeID, f.MissingPath, f.Content)
			}
			for _, grp := range dupGroups {
				fmt.Fprintf(&sum, "  duplicate nodes: %s (consider `soma memory merge`)\n", strings.Join(grp, ","))
			}

			itemID := fmt.Sprintf("inb_cons_%d", time.Now().UnixNano())
			if err := st.InsertInboxItem(ctx, store.InboxItem{
				ID:       itemID,
				ItemKind: "consolidation_report",
				Title:    fmt.Sprintf("Memory consolidation audit (%d stale, %d duplicate groups)", len(stale), len(dupGroups)),
				Summary:  strings.TrimRight(sum.String(), "\n"),
				Urgency:  "low",
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Staged consolidation report %s\n%s", itemID, sum.String())
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&scope, "scope", "", "Filter memory scope")
	return cmd
}

func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitCommaInt64(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func serveHTTPBridge(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
