package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultTelegramAPIBase = "https://api.telegram.org"

// TelegramConfig configures the out-of-process Telegram Bot API Channel Bridge adapter.
type TelegramConfig struct {
	BotToken       string
	APIBaseURL     string
	AllowedUserIDs []int64
	AllowedChatIDs []int64
	AlertChatID    int64
	PollInterval   time.Duration
}

// TelegramAdapter polls Telegram updates and dispatches authorized commands strictly through KernelClient.
type TelegramAdapter struct {
	Client     *KernelClient
	Config     TelegramConfig
	Pairing    *PairingStore
	HTTPClient *http.Client

	mu        sync.Mutex
	offset    int64
	seenInbox map[string]bool
}

// NewTelegramAdapter constructs a fail-closed TelegramAdapter talking strictly to client.
func NewTelegramAdapter(client *KernelClient, cfg TelegramConfig) *TelegramAdapter {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultTelegramAPIBase
	}
	cfg.APIBaseURL = strings.TrimRight(cfg.APIBaseURL, "/")
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return &TelegramAdapter{
		Client:     client,
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 40 * time.Second},
		seenInbox:  make(map[string]bool),
	}
}

// Name returns "telegram" to satisfy ChannelAdapter.
func (a *TelegramAdapter) Name() string { return "telegram" }

// Send delivers an OutboundMessage to Telegram via SendMessage.
func (a *TelegramAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	var chatID int64
	if strings.TrimSpace(msg.Target) != "" {
		chatID, _ = strconv.ParseInt(strings.TrimSpace(msg.Target), 10, 64)
	}
	return a.SendMessage(ctx, chatID, msg.Text)
}

func (a *TelegramAdapter) isUserAllowed(userID int64) bool {
	if userID == 0 {
		return false
	}
	for _, id := range a.Config.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	if a.Pairing != nil && a.Pairing.IsApproved("telegram", strconv.FormatInt(userID, 10)) {
		return true
	}
	return false
}

func (a *TelegramAdapter) isChatAllowed(chatID int64) bool {
	if len(a.Config.AllowedChatIDs) == 0 {
		return true
	}
	for _, id := range a.Config.AllowedChatIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

type tgUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message,omitempty"`
}

type tgMessage struct {
	MessageID int64 `json:"message_id"`
	From      *struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from,omitempty"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

// PollUpdatesOnce fetches pending Telegram updates, advances offset, enforces allowlists, and replies.
func (a *TelegramAdapter) PollUpdatesOnce(ctx context.Context) (int, error) {
	return a.pollUpdates(ctx, 0)
}

// pollUpdates fetches updates, long-polling for up to waitSeconds when no update is pending.
func (a *TelegramAdapter) pollUpdates(ctx context.Context, waitSeconds int) (int, error) {
	a.mu.Lock()
	off := a.offset
	a.mu.Unlock()

	url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=%d", a.Config.APIBaseURL, a.Config.BotToken, off, waitSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("telegram getUpdates status %d", resp.StatusCode)
	}
	var parsed tgUpdatesResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed); err != nil {
		return 0, err
	}
	if !parsed.OK {
		return 0, errors.New("telegram getUpdates ok=false")
	}

	processed := 0
	var firstSendErr error
	for _, upd := range parsed.Result {
		// Acknowledge each update before acting on it. If a reply later fails (rate limit, user
		// blocked the bot) the update must not be fetched and executed again on the next poll.
		a.mu.Lock()
		if upd.UpdateID >= a.offset {
			a.offset = upd.UpdateID + 1
		}
		a.mu.Unlock()
		if upd.Message == nil || upd.Message.From == nil {
			continue
		}
		userID := upd.Message.From.ID
		chatID := upd.Message.Chat.ID
		if !a.isChatAllowed(chatID) {
			continue
		}
		if !a.isUserAllowed(userID) {
			if a.Pairing != nil && chatID == userID && strings.TrimSpace(upd.Message.Text) != "" {
				if req, pErr := a.Pairing.IssueCode("telegram", strconv.FormatInt(userID, 10), upd.Message.From.Username); pErr == nil {
					_ = a.SendMessage(ctx, chatID, fmt.Sprintf("Pairing required. Approve code %s in soma Dashboard → Connectors → Channel Bridges (or run `soma bridge pair %s`).", req.Code, req.Code))
				}
			}
			continue
		}
		cmd, ok := ParseCommand(upd.Message.Text)
		var reply string
		var cmdErr error
		if ok {
			reply, cmdErr = dispatchBridgeCommand(ctx, a.Client, "telegram", fmt.Sprintf("telegram:%d", userID), "/", cmd)
		} else if strings.TrimSpace(upd.Message.Text) != "" && chatID == userID && a.Client != nil {
			res, sErr := a.Client.StartRun(ctx, StartRunRequest{
				Goal:   strings.TrimSpace(upd.Message.Text),
				Source: fmt.Sprintf("telegram:%d", chatID),
				Sync:   true,
			})
			if sErr != nil {
				cmdErr = sErr
			} else if strings.TrimSpace(res.Summary) != "" {
				reply = res.Summary
			} else {
				reply = fmt.Sprintf("run %s (%s)", res.RunID, res.Status)
			}
		} else {
			continue
		}
		if cmdErr != nil {
			reply = "error: " + cmdErr.Error()
		}
		if err := a.SendMessage(ctx, chatID, reply); err != nil {
			if firstSendErr == nil {
				firstSendErr = err
			}
			continue
		}
		processed++
	}
	return processed, firstSendErr
}

// SendMessage sends chunked Telegram replies (max 4000 chars) to chatID.
func (a *TelegramAdapter) SendMessage(ctx context.Context, chatID int64, text string) error {
	if chatID == 0 {
		chatID = a.Config.AlertChatID
	}
	if chatID == 0 {
		return errors.New("telegram: chatID is required")
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", a.Config.APIBaseURL, a.Config.BotToken)
	for _, chunk := range ChunkMessage(text, 4000) {
		raw, _ := json.Marshal(map[string]any{
			"chat_id": chatID,
			"text":    chunk,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("telegram sendMessage status %d", resp.StatusCode)
		}
	}
	return nil
}

// PollInboxOnce checks KernelClient for unseen pending inbox items and sends alert messages to AlertChatID.
func (a *TelegramAdapter) PollInboxOnce(ctx context.Context) (int, error) {
	if a.Config.AlertChatID == 0 {
		return 0, nil
	}
	items, err := a.Client.ListInbox(ctx, "pending")
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, it := range items {
		a.mu.Lock()
		seen := a.seenInbox[it.ID]
		if !seen {
			a.seenInbox[it.ID] = true
		}
		a.mu.Unlock()
		if seen {
			continue
		}
		msg := fmt.Sprintf("[soma inbox:%s] %s\n%s\n/approve %s | /dismiss %s",
			it.Urgency, it.Title, it.Summary, it.ID, it.ID)
		if err := a.SendMessage(ctx, a.Config.AlertChatID, msg); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// Run polls Telegram updates and inbox alerts until ctx cancels.
func (a *TelegramAdapter) Run(ctx context.Context) error {
	// Inbox alerts tick on their own so a long-poll (or a long-running message) does not delay them.
	go func() {
		ticker := time.NewTicker(max(a.Config.PollInterval, 5*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = a.PollInboxOnce(ctx)
			}
		}
	}()

	// Long-poll: one held request instead of a request every few seconds, with instant delivery.
	wait := 25
	if a.HTTPClient != nil && a.HTTPClient.Timeout > 0 && a.HTTPClient.Timeout < 35*time.Second {
		wait = 0
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		_, err := a.pollUpdates(ctx, wait)
		if err != nil || wait == 0 {
			pause := a.Config.PollInterval
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pause):
			}
		}
	}
}
