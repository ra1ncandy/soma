package webui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/bridge"
	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/dag"
)

// TriggerRunSummaryDTO reports the latest daemon execution status for a configured trigger.
type TriggerRunSummaryDTO struct {
	Status    string `json:"status"`
	BranchID  string `json:"branch_id,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// RuntimeSupervisor supervises soma web's embedded Tier-0 Daemon and active Channel Bridge adapters,
// hot-reloading workers whenever .soma/triggers.yaml or .soma/harness.json changes.
type RuntimeSupervisor struct {
	server *Server

	mu            sync.RWMutex
	started       bool
	parentCtx     context.Context
	epochCancel   context.CancelFunc
	epochWG       sync.WaitGroup
	daemonRunning bool
	daemonInst    *daemon.Daemon
	gateway       *bridge.Gateway
	webhookRoutes map[string]http.Handler
	bridgeRunning map[string]bool
	bridgeErrors  map[string]string
}

func newRuntimeSupervisor(s *Server) *RuntimeSupervisor {
	rs := &RuntimeSupervisor{
		server:        s,
		webhookRoutes: make(map[string]http.Handler),
		bridgeRunning: make(map[string]bool),
		bridgeErrors:  make(map[string]string),
	}
	rs.rebuildHandlersOnly()
	return rs
}

// Start begins background supervision of the embedded Daemon, KernelServer IPC socket, and polling Bridges.
func (rs *RuntimeSupervisor) Start(ctx context.Context) {
	rs.mu.Lock()
	rs.started = true
	rs.parentCtx = ctx
	rs.mu.Unlock()
	rs.Reload()
}

// Stop halts all supervised Daemon and Bridge goroutines cleanly.
func (rs *RuntimeSupervisor) Stop() {
	rs.mu.Lock()
	rs.started = false
	cancel := rs.epochCancel
	rs.epochCancel = nil
	rs.daemonRunning = false
	rs.bridgeRunning = make(map[string]bool)
	rs.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	rs.epochWG.Wait()
}

// Reload rebuilds webhook handlers and, when Start has been called, restarts background workers cleanly.
func (rs *RuntimeSupervisor) Reload() {
	rs.mu.Lock()
	isStarted := rs.started
	parentCtx := rs.parentCtx
	prevCancel := rs.epochCancel
	rs.epochCancel = nil
	rs.daemonRunning = false
	rs.bridgeRunning = make(map[string]bool)
	rs.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
		rs.epochWG.Wait()
	}

	if !isStarted || parentCtx == nil || parentCtx.Err() != nil {
		rs.rebuildHandlersOnly()
		return
	}

	epochCtx, cancel := context.WithCancel(parentCtx)
	hCfg := rs.server.loadHarnessConfig()
	dCfg, err := daemon.LoadConfig(rs.server.triggersPath())
	if err != nil {
		dCfg = daemon.DefaultConfig()
	}

	somaDir := rs.server.store.SomaDir()
	prov := rs.server.activeProvider()

	ks, ksErr := bridge.NewKernelServer(rs.server.store, bridge.ServerConfig{
		SomaDir:      somaDir,
		WorkspaceDir: rs.server.cfg.WorkspaceDir,
		Version:      rs.server.cfg.Version,
		Provider:     prov,
		RunStarter:   rs.runFromBridge,
		BranchResumer: func(ctx context.Context, branchID string) error {
			br, err := rs.server.store.GetBranch(ctx, branchID)
			if err != nil {
				return err
			}
			exec, _, err := rs.server.executorForRequest(ctx, "worker", br.ActiveAccountID)
			if err != nil {
				return err
			}
			_ = rs.server.store.UpdateBranchStatus(ctx, br.ID, string(dag.StatusRunning), br.ActiveAccountID, br.StallCount)
			rs.server.runBranchAsync(exec, br.ID, rs.server.defaultMaxTurns())
			return nil
		},
	})
	var kClient *bridge.KernelClient
	if ksErr == nil {
		kClient = bridge.NewClient(ks.SocketPath(), ks.Token())
		rs.epochWG.Add(1)
		go func() {
			defer rs.epochWG.Done()
			_ = ks.Start(epochCtx)
		}()
	}

	gw, routes, pollers := rs.configureBridges(hCfg.Bridges, kClient, somaDir, true)

	d := daemon.New(rs.server.store, dCfg, rs.server.cfg.WorkspaceDir)
	d.Provider = prov
	d.Profile = compiler.WorkerProfile()
	d.OutboundNotify = func(c context.Context, channel, target, text string) error {
		if gw == nil {
			return nil
		}
		return gw.Deliver(c, bridge.OutboundMessage{
			Channel: channel,
			Target:  target,
			Text:    text,
		})
	}
	d.OnOutcome = func(_ daemon.TriggerOutcome) {
		rs.server.notify()
	}

	rs.mu.Lock()
	rs.epochCancel = cancel
	rs.daemonInst = d
	rs.gateway = gw
	rs.webhookRoutes = routes
	rs.daemonRunning = true
	rs.mu.Unlock()

	rs.epochWG.Add(1)
	go func() {
		defer rs.epochWG.Done()
		_ = d.StartWorkers(epochCtx)
	}()

	// Idle auto-stop watcher for Virtual Desktop (5m inactivity, paused while tasks run or Take Over is active).
	rs.epochWG.Add(1)
	go func() {
		defer rs.epochWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-epochCtx.Done():
				return
			case <-ticker.C:
				rs.server.maybeAutoStopDesktop(epochCtx, 5*time.Minute)
			}
		}
	}()

	for chName, fn := range pollers {
		name := chName
		runFn := fn
		rs.setBridgeRunning(name, true, "")
		rs.epochWG.Add(1)
		go func() {
			defer rs.epochWG.Done()
			err := runFn(epochCtx)
			if err != nil && epochCtx.Err() == nil {
				rs.setBridgeRunning(name, false, err.Error())
				rs.server.notify()
			}
		}()
	}
}

func (rs *RuntimeSupervisor) rebuildHandlersOnly() {
	hCfg := rs.server.loadHarnessConfig()
	dCfg, err := daemon.LoadConfig(rs.server.triggersPath())
	if err != nil {
		dCfg = daemon.DefaultConfig()
	}
	somaDir := rs.server.store.SomaDir()
	gw, routes, _ := rs.configureBridges(hCfg.Bridges, nil, somaDir, false)
	d := daemon.New(rs.server.store, dCfg, rs.server.cfg.WorkspaceDir)
	d.Provider = rs.server.activeProvider()
	d.OnOutcome = func(_ daemon.TriggerOutcome) {
		rs.server.notify()
	}

	rs.mu.Lock()
	rs.daemonInst = d
	rs.gateway = gw
	rs.webhookRoutes = routes
	rs.mu.Unlock()
}

func (rs *RuntimeSupervisor) configureBridges(
	br BridgesConfig,
	client *bridge.KernelClient,
	somaDir string,
	markWebhookRunning bool,
) (*bridge.Gateway, map[string]http.Handler, map[string]func(context.Context) error) {
	gw := bridge.NewGateway(client, somaDir)
	ps := gw.Pairing()
	routes := make(map[string]http.Handler)
	pollers := make(map[string]func(context.Context) error)

	mark := func(ch string) {
		if markWebhookRunning {
			rs.setBridgeRunning(ch, true, "")
		}
	}

	if br.Discord.Enabled {
		adp := bridge.NewDiscordAdapter(client, bridge.DiscordConfig{
			BotToken:          br.Discord.BotToken,
			PublicKeyHex:      br.Discord.PublicKeyHex,
			ChannelID:         br.Discord.ChannelID,
			AllowedUserIDs:    br.Discord.AllowedUserIDs,
			AllowedChannelIDs: br.Discord.AllowedChannelIDs,
		})
		adp.Pairing = ps
		gw.RegisterAdapter(adp)
		routes["discord"] = adp.InteractionHandler()
		if strings.TrimSpace(br.Discord.BotToken) != "" && strings.TrimSpace(br.Discord.ChannelID) != "" && client != nil {
			pollers["discord"] = adp.Run
		} else {
			mark("discord")
		}
	}

	if br.Telegram.Enabled && strings.TrimSpace(br.Telegram.BotToken) != "" {
		adp := bridge.NewTelegramAdapter(client, bridge.TelegramConfig{
			BotToken:       br.Telegram.BotToken,
			AllowedUserIDs: br.Telegram.AllowedUserIDs,
			AllowedChatIDs: br.Telegram.AllowedChatIDs,
			AlertChatID:    br.Telegram.AlertChatID,
		})
		adp.Pairing = ps
		gw.RegisterAdapter(adp)
		if client != nil {
			pollers["telegram"] = adp.Run
		}
	}

	if br.Slack.Enabled {
		adp := bridge.NewSlackAdapter(gw, bridge.SlackConfig{
			BotToken:          br.Slack.BotToken,
			SigningSecret:     br.Slack.SigningSecret,
			AllowedUserIDs:    br.Slack.AllowedUserIDs,
			AllowedChannelIDs: br.Slack.AllowedChannelIDs,
		})
		gw.RegisterAdapter(adp)
		routes["slack"] = adp.Handler()
		mark("slack")
	}

	if br.Matrix.Enabled && strings.TrimSpace(br.Matrix.HomeserverURL) != "" && strings.TrimSpace(br.Matrix.AccessToken) != "" {
		adp := bridge.NewMatrixAdapter(gw, bridge.MatrixConfig{
			HomeserverURL:  br.Matrix.HomeserverURL,
			AccessToken:    br.Matrix.AccessToken,
			UserID:         br.Matrix.UserID,
			AllowedSenders: br.Matrix.AllowedSenders,
			AllowedRooms:   br.Matrix.AllowedRooms,
		})
		gw.RegisterAdapter(adp)
		if client != nil {
			pollers["matrix"] = adp.Run
		}
	}

	if br.Mattermost.Enabled {
		adp := bridge.NewMattermostAdapter(gw, bridge.MattermostConfig{
			ServerURL:      br.Mattermost.ServerURL,
			BotToken:       br.Mattermost.BotToken,
			WebhookURL:     br.Mattermost.WebhookURL,
			OutgoingToken:  br.Mattermost.OutgoingToken,
			AllowedUserIDs: br.Mattermost.AllowedUserIDs,
		})
		gw.RegisterAdapter(adp)
		routes["mattermost"] = adp.Handler()
		mark("mattermost")
	}

	if br.Teams.Enabled {
		adp := bridge.NewTeamsAdapter(gw, br.Teams.WebhookURL, br.Teams.AllowedUserIDs)
		gw.RegisterAdapter(adp)
		mark("teams")
	}

	if br.GoogleChat.Enabled {
		adp := bridge.NewGoogleChatAdapter(gw, br.GoogleChat.WebhookURL, br.GoogleChat.AllowedUserIDs)
		gw.RegisterAdapter(adp)
		routes["google-chat"] = adp.Handler()
		routes["google_chat"] = adp.Handler()
		mark("google_chat")
	}

	if br.Email.Enabled && strings.TrimSpace(br.Email.SMTPHost) != "" {
		adp := bridge.NewEmailAdapter(bridge.EmailConfig{
			SMTPHost: br.Email.SMTPHost,
			SMTPPort: br.Email.SMTPPort,
			Username: br.Email.Username,
			Password: br.Email.Password,
			From:     br.Email.From,
		})
		gw.RegisterAdapter(adp)
		mark("email")
	}

	for _, sc := range []struct {
		name string
		cfg  SidecarBridgeCfg
	}{
		{"whatsapp", br.WhatsApp},
		{"signal", br.Signal},
		{"sms", br.SMS},
	} {
		if sc.cfg.Enabled {
			adp := bridge.NewWebhookBridgeAdapter(gw, bridge.WebhookBridgeConfig{
				ChannelName:    sc.name,
				OutboundURL:    sc.cfg.OutboundURL,
				BearerToken:    sc.cfg.BearerToken,
				HMACSecret:     sc.cfg.HMACSecret,
				AllowedSenders: sc.cfg.AllowedSenders,
			})
			gw.RegisterAdapter(adp)
			routes[sc.name] = adp.Handler()
			mark(sc.name)
		}
	}

	return gw, routes, pollers
}

func (rs *RuntimeSupervisor) setBridgeRunning(ch string, running bool, errMsg string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.bridgeRunning == nil {
		rs.bridgeRunning = make(map[string]bool)
	}
	if rs.bridgeErrors == nil {
		rs.bridgeErrors = make(map[string]string)
	}
	rs.bridgeRunning[ch] = running
	if errMsg != "" {
		rs.bridgeErrors[ch] = errMsg
	} else {
		delete(rs.bridgeErrors, ch)
	}
}

// StatusSnapshot returns secret-safe runtime status for Daemon and Channel Bridges.
func (rs *RuntimeSupervisor) StatusSnapshot() (daemonRunning bool, running map[string]bool, errs map[string]string) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	runCopy := make(map[string]bool, len(rs.bridgeRunning))
	for k, v := range rs.bridgeRunning {
		runCopy[k] = v
	}
	errCopy := make(map[string]string, len(rs.bridgeErrors))
	for k, v := range rs.bridgeErrors {
		errCopy[k] = v
	}
	return rs.daemonRunning, runCopy, errCopy
}

// ServeTriggerWebhook dispatches /webhooks/triggers/{id} to the active Daemon WebhookHandler.
func (rs *RuntimeSupervisor) ServeTriggerWebhook(w http.ResponseWriter, r *http.Request) {
	rs.mu.RLock()
	d := rs.daemonInst
	rs.mu.RUnlock()
	if d == nil {
		http.Error(w, "daemon unavailable", http.StatusServiceUnavailable)
		return
	}
	clone := r.Clone(r.Context())
	trigID := strings.TrimPrefix(r.URL.Path, "/webhooks/triggers/")
	clone.URL.Path = "/webhook/" + strings.TrimPrefix(trigID, "/")
	d.WebhookHandler().ServeHTTP(w, clone)
}

// ServeBridgeWebhook dispatches /webhooks/bridges/{channel} to the active Channel Bridge HTTP handler.
func (rs *RuntimeSupervisor) ServeBridgeWebhook(w http.ResponseWriter, r *http.Request) {
	ch := strings.TrimPrefix(r.URL.Path, "/webhooks/bridges/")
	ch = strings.Trim(strings.ToLower(ch), "/")
	rs.mu.RLock()
	h := rs.webhookRoutes[ch]
	rs.mu.RUnlock()
	if h == nil {
		http.Error(w, fmt.Sprintf("bridge webhook %q is not enabled", ch), http.StatusNotFound)
		return
	}
	h.ServeHTTP(w, r)
}

func (rs *RuntimeSupervisor) runFromBridge(ctx context.Context, req bridge.StartRunRequest) (bridge.StartRunResponse, error) {
	hCfg := rs.server.loadHarnessConfig()
	maxTurns := hCfg.Provider.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 15
	}
	title := req.Source
	if title != "" {
		title = "[" + title + "] " + firstLine(req.Goal, 48)
	}
	if !req.Sync {
		runID, branchID, err := rs.server.launchAsyncRun(ctx, title, req.Goal, req.Profile, maxTurns)
		if err != nil {
			return bridge.StartRunResponse{}, err
		}
		return bridge.StartRunResponse{
			RunID:    runID,
			BranchID: branchID,
			Status:   "running",
		}, nil
	}

	exec, accIDs, err := rs.server.executorForRequest(ctx, req.Profile, "")
	if err != nil {
		return bridge.StartRunResponse{}, err
	}
	prof := compiler.WorkerProfile()
	if strings.EqualFold(req.Profile, "frontier") {
		prof = compiler.FrontierProfile()
	}
	defer closeExecutor(exec)
	runID, branchID, err := exec.StartRun(ctx, rs.server.buildRunRequest(title, req.Goal, prof, accIDs))
	if err != nil {
		return bridge.StartRunResponse{}, err
	}
	rs.server.notify()
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, runErr := exec.ExecuteBranch(runCtx, branchID, maxTurns)
	rs.server.notify()
	if runErr != nil {
		return bridge.StartRunResponse{
			RunID:    runID,
			BranchID: branchID,
			Status:   "failed",
		}, runErr
	}
	status := "completed"
	summary := ""
	if res != nil {
		status = string(res.Status)
		summary = res.FinalSummary
	}
	return bridge.StartRunResponse{
		RunID:    runID,
		BranchID: branchID,
		Status:   status,
		Summary:  summary,
	}, nil
}

// ParseInt64List parses comma-separated integers cleanly for CLI/bridge config helpers.
func ParseInt64List(items []string) []int64 {
	var out []int64
	for _, raw := range items {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n != 0 {
			out = append(out, n)
		}
	}
	return out
}
