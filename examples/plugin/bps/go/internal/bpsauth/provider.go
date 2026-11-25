package bpsauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxSessions = 128

type loginSession struct {
	mu      sync.Mutex
	client  *DeviceClient
	flow    deviceFlow
	expires time.Time
	next    time.Time
	result  *pluginapi.AuthData
	err     error
}

type Provider struct {
	mu        sync.Mutex
	sessions  map[string]*loginSession
	starting  int
	now       func() time.Time
	newClient func(string) (*DeviceClient, error)
	ctx       context.Context
	cancel    context.CancelFunc
}

func New() *Provider {
	ctx, cancel := context.WithCancel(context.Background())
	return &Provider{sessions: make(map[string]*loginSession), now: time.Now,
		newClient: NewDeviceClient, ctx: ctx, cancel: cancel}
}

func (*Provider) Identifier() string { return "bps" }

func (p *Provider) Close() {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	for state, session := range p.sessions {
		session.client.Close()
		delete(p.sessions, state)
	}
}

func (p *Provider) requestContext(ctx context.Context, deadline time.Time) (context.Context, func()) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(p.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (p *Provider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if method, _ := req.Metadata["method"].(string); method != "" && method != "device" {
		return pluginapi.AuthLoginStartResponse{}, errors.New("BPS plugin supports method=device")
	}
	now := p.now()
	p.mu.Lock()
	for state, session := range p.sessions {
		if !session.expires.After(now) {
			session.client.Close()
			delete(p.sessions, state)
		}
	}
	if p.ctx.Err() != nil || len(p.sessions)+p.starting >= maxSessions {
		p.mu.Unlock()
		return pluginapi.AuthLoginStartResponse{}, errors.New("BPS login is unavailable or too many sessions are pending")
	}
	p.starting++
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.starting--; p.mu.Unlock() }()
	client, errClient := p.newClient(req.Host.ProxyURL)
	if errClient != nil {
		return pluginapi.AuthLoginStartResponse{}, errClient
	}
	var nonce [32]byte
	if _, errRandom := rand.Read(nonce[:]); errRandom != nil {
		client.Close()
		return pluginapi.AuthLoginStartResponse{}, errors.New("cannot initialize BPS login")
	}
	expires := now.Add(loginLifetime)
	requestCtx, cancel := p.requestContext(ctx, expires)
	defer cancel()
	flow, errStart := client.start(requestCtx)
	if errStart != nil {
		client.Close()
		return pluginapi.AuthLoginStartResponse{}, errStart
	}
	state := "bps-" + hex.EncodeToString(nonce[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		client.Close()
		return pluginapi.AuthLoginStartResponse{}, context.Canceled
	}
	p.sessions[state] = &loginSession{client: client, flow: flow, expires: expires, next: p.now().Add(flow.interval)}
	return pluginapi.AuthLoginStartResponse{Provider: "bps", State: state, URL: flow.url,
		UserCode: flow.code, Interval: int(flow.interval / time.Second), ExpiresAt: expires}, nil
}

func (p *Provider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	p.mu.Lock()
	session := p.sessions[req.State]
	p.mu.Unlock()
	if session == nil {
		return pluginapi.AuthLoginPollResponse{}, errors.New("unknown or expired BPS login session; start a new login")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.expires.After(p.now()) || p.ctx.Err() != nil {
		return pluginapi.AuthLoginPollResponse{}, errors.New("BPS login expired or stopped")
	}
	if session.err != nil {
		return pluginapi.AuthLoginPollResponse{}, session.err
	}
	if session.result != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: *session.result}, nil
	}
	if p.now().Before(session.next) {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	requestCtx, cancel := p.requestContext(ctx, session.expires)
	defer cancel()
	credentials, errPoll := session.client.poll(requestCtx, &session.flow)
	session.next = p.now().Add(session.flow.interval)
	if errPoll != nil {
		// Do not replay a possibly consumed authorization code after an ambiguous exchange.
		session.err = errPoll
		session.flow.id, session.flow.code = "", ""
		session.client.Close()
		return pluginapi.AuthLoginPollResponse{}, errPoll
	}
	if credentials == nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	credential, errNormalize := normalizeCredential(*credentials, "", "")
	if errNormalize != nil {
		session.err = errNormalize
		return pluginapi.AuthLoginPollResponse{}, errNormalize
	}
	metadata := map[string]any{}
	if req.Host.ProxyURL != "" {
		metadata["proxy_url"] = req.Host.ProxyURL
	}
	auth, errAuth := authData(credential, metadata)
	if errAuth != nil {
		session.err = errAuth
		return pluginapi.AuthLoginPollResponse{}, errAuth
	}
	session.result = &auth
	session.flow.id, session.flow.code = "", ""
	session.client.Close()
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth}, nil
}

func (*Provider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	if req.Provider != "" && req.Provider != "bps" {
		return pluginapi.AuthParseResponse{}, nil
	}
	credential, metadata, handled, err := parseCredential(req.RawJSON, req.Provider == "bps")
	if !handled || err != nil {
		return pluginapi.AuthParseResponse{Handled: handled}, err
	}
	auth, errAuth := authData(credential, metadata)
	if req.FileName != "" {
		auth.ID, auth.FileName = req.FileName, req.FileName
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, errAuth
}

// RefreshAuth is used by CPA's manual refresh endpoint. No refresh lead or
// interval is registered, so this first version does not schedule automatic refresh.
func (p *Provider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	if req.AuthProvider != "bps" {
		return pluginapi.AuthRefreshResponse{}, errors.New("BPS refresh requires a BPS credential")
	}
	credential, metadata, _, err := parseCredential(req.StorageJSON, true)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	proxyURL, _ := metadata["proxy_url"].(string)
	if proxyURL == "" {
		proxyURL = req.Host.ProxyURL
	}
	client, errClient := p.newClient(proxyURL)
	if errClient != nil {
		return pluginapi.AuthRefreshResponse{}, errClient
	}
	defer client.Close()
	requestCtx, cancel := p.requestContext(ctx, p.now().Add(30*time.Second))
	defer cancel()
	credentials, errRefresh := client.Refresh(requestCtx, credential.DeviceCredentials)
	if errRefresh != nil {
		return pluginapi.AuthRefreshResponse{}, errRefresh
	}
	next, errNormalize := normalizeCredential(*credentials, credential.AccountID, "")
	if errNormalize != nil {
		return pluginapi.AuthRefreshResponse{}, errNormalize
	}
	if credential.Subject != "" && next.Subject != credential.Subject {
		return pluginapi.AuthRefreshResponse{}, errors.New("BPS refresh changed user identity")
	}
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	auth, errAuth := authData(next, metadata)
	if req.AuthID != "" {
		auth.ID = req.AuthID
		// The host preserves the original filename when this field is omitted.
		auth.FileName = ""
	}
	return pluginapi.AuthRefreshResponse{Auth: auth}, errAuth
}
