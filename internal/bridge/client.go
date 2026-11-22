package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// KernelClient communicates with soma's KernelServer strictly over an authenticated Unix Domain Socket.
type KernelClient struct {
	socketPath string
	token      string
	httpClient *http.Client
}

// NewClient constructs a KernelClient targeting socketPath with bearer token.
func NewClient(socketPath, token string) *KernelClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &KernelClient{
		socketPath: socketPath,
		token:      token,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		},
	}
}

func (c *KernelClient) doJSON(ctx context.Context, method, path string, reqBody, dst any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://soma-kernel"+path, bodyReader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var er ErrorResponse
		if json.Unmarshal(data, &er) == nil && er.Error != "" {
			return fmt.Errorf("bridge http %d: %s", resp.StatusCode, er.Error)
		}
		return fmt.Errorf("bridge http %d: %s", resp.StatusCode, string(data))
	}
	if dst != nil && len(data) > 0 {
		return json.Unmarshal(data, dst)
	}
	return nil
}

// Health verifies kernel socket reachability and authentication.
func (c *KernelClient) Health(ctx context.Context) (HealthResponse, error) {
	var out HealthResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &out)
	return out, err
}

// Status returns runs, branches, accounts, and pending inbox count.
func (c *KernelClient) Status(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

// ListInbox lists operator inbox items filtered by status (default "pending", or "all").
func (c *KernelClient) ListInbox(ctx context.Context, status string) ([]store.InboxItem, error) {
	q := ""
	if status != "" {
		q = "?status=" + url.QueryEscape(status)
	}
	var out InboxListResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/inbox"+q, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ApproveInbox approves inbox item id via shared kernel service.
func (c *KernelClient) ApproveInbox(ctx context.Context, id, actor string) (InboxActionResponse, error) {
	var out InboxActionResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/inbox/"+url.PathEscape(id)+"/approve", InboxActionRequest{Actor: actor}, &out)
	return out, err
}

// DismissInbox dismisses inbox item id via shared kernel service.
func (c *KernelClient) DismissInbox(ctx context.Context, id, actor string) (InboxActionResponse, error) {
	var out InboxActionResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/inbox/"+url.PathEscape(id)+"/dismiss", InboxActionRequest{Actor: actor}, &out)
	return out, err
}

// StartRun requests execution of a new task run on the kernel.
func (c *KernelClient) StartRun(ctx context.Context, req StartRunRequest) (StartRunResponse, error) {
	var out StartRunResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/runs", req, &out)
	return out, err
}

// Reconcile resolves an ambiguous action on branchID.
func (c *KernelClient) Reconcile(ctx context.Context, branchID string, req ReconcileRequest) (ReconcileResponse, error) {
	var out ReconcileResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/branches/"+url.PathEscape(branchID)+"/reconcile", req, &out)
	return out, err
}
