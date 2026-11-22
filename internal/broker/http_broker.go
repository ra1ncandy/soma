package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EndpointPolicy constrains outbound HTTP requests and injects host-held auth headers.
type EndpointPolicy struct {
	Domain             string // host or host:port
	Scheme             string // defaults to "https"
	PathPrefixes       []string
	AllowedMethods     []string
	AuthHeaderName     string
	AuthHeaderValue    string
	EnforcedJSONFields map[string]any
}

// HTTPBroker mediates outbound API requests on the host without exposing secrets to the sandbox.
type HTTPBroker struct {
	Policies   map[string]EndpointPolicy
	HTTPClient *http.Client
}

// NewHTTPBroker initializes an HTTPBroker from a slice of EndpointPolicies keyed by Domain.
func NewHTTPBroker(policies ...EndpointPolicy) *HTTPBroker {
	pm := make(map[string]EndpointPolicy, len(policies))
	for _, p := range policies {
		pm[strings.ToLower(p.Domain)] = p
	}
	return &HTTPBroker{
		Policies: pm,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (h *HTTPBroker) Name() string { return "http" }

func (h *HTTPBroker) Execute(ctx context.Context, req BrokerRequest) (BrokerResponse, error) {
	targetStr := strings.TrimSpace(req.Target)
	if !strings.Contains(targetStr, "://") {
		targetStr = "https://" + targetStr
	}
	u, err := url.Parse(targetStr)
	if err != nil {
		return BrokerResponse{}, fmt.Errorf("http broker invalid target %q: %w", req.Target, err)
	}

	pol, ok := h.Policies[strings.ToLower(u.Host)]
	if !ok {
		pol, ok = h.Policies[strings.ToLower(u.Hostname())]
	}
	if !ok {
		return BrokerResponse{}, fmt.Errorf("%w: domain %q not allowlisted", ErrPolicyDenied, u.Host)
	}

	scheme := pol.Scheme
	if scheme == "" {
		scheme = "https"
	}
	u.Scheme = scheme

	method := strings.ToUpper(strings.TrimSpace(req.Op))
	if method == "" {
		method = http.MethodGet
	}
	if !containsFold(pol.AllowedMethods, method) {
		return BrokerResponse{}, fmt.Errorf("%w: method %s not allowed for %s", ErrPolicyDenied, method, pol.Domain)
	}
	if len(pol.PathPrefixes) > 0 && !matchesAnyPrefix(u.Path, pol.PathPrefixes) {
		return BrokerResponse{}, fmt.Errorf("%w: path %q outside allowlisted prefixes for %s", ErrPolicyDenied, u.Path, pol.Domain)
	}

	var bodyReader io.Reader
	bodyBytes := []byte(req.ParamsJSON)
	if len(pol.EnforcedJSONFields) > 0 {
		obj := make(map[string]any)
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			if err := json.Unmarshal(bodyBytes, &obj); err != nil {
				return BrokerResponse{}, fmt.Errorf("%w: body must be valid JSON object when EnforcedJSONFields set: %v", ErrPolicyDenied, err)
			}
		}
		for k, v := range pol.EnforcedJSONFields {
			obj[k] = v
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			return BrokerResponse{}, err
		}
		bodyBytes = encoded
	}
	if len(bytes.TrimSpace(bodyBytes)) > 0 && method != http.MethodGet && method != http.MethodHead {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return BrokerResponse{}, err
	}
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if pol.AuthHeaderName != "" && pol.AuthHeaderValue != "" {
		httpReq.Header.Set(pol.AuthHeaderName, pol.AuthHeaderValue)
	}
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

	client := h.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return BrokerResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return BrokerResponse{}, err
	}

	hdrs := make(map[string]string)
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			hdrs[k] = vals[0]
		}
	}

	return BrokerResponse{
		StatusCode:  resp.StatusCode,
		Body:        raw,
		ContentType: resp.Header.Get("Content-Type"),
		TrustTier:   "untrusted_external",
		Headers:     hdrs,
	}, nil
}

func containsFold(slice []string, item string) bool {
	if len(slice) == 0 {
		return true
	}
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}

func matchesAnyPrefix(path string, prefixes []string) bool {
	if path == "" {
		path = "/"
	}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

var _ = errors.Is
