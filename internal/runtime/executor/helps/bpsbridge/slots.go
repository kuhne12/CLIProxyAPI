package bpsbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// SlotPool bounds concurrent tasks per account. Stable identities survive CPA
// restarts; every call still sends its complete, caller-isolated history.
type SlotPool struct {
	mu       sync.Mutex
	accounts map[string]*accountSlots
}

type accountSlots struct {
	available chan int
	users     int
}

func (p *SlotPool) Acquire(ctx context.Context, account string) (PrepareOptions, func(), error) {
	p.mu.Lock()
	if p.accounts == nil {
		p.accounts = make(map[string]*accountSlots)
	}
	slots := p.accounts[account]
	if slots == nil {
		slots = &accountSlots{available: make(chan int, 4)}
		for i := 0; i < 4; i++ {
			slots.available <- i
		}
		p.accounts[account] = slots
	}
	if slots.users >= 68 {
		p.mu.Unlock()
		return PrepareOptions{}, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeTransientRateLimit, HTTPStatus: 429, RetryAfterSeconds: 1, Message: "BPS request queue is full"}
	}
	slots.users++
	p.mu.Unlock()
	done := func() {
		p.mu.Lock()
		slots.users--
		if slots.users == 0 {
			delete(p.accounts, account)
		}
		p.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		done()
		return PrepareOptions{}, nil, ctx.Err()
	case slot := <-slots.available:
		var once sync.Once
		release := func() { once.Do(func() { slots.available <- slot; done() }) }
		task := sha256.Sum256([]byte(fmt.Sprintf("cpa-bps-task-v1\x00%s\x00%d", account, slot)))
		turn := sha256.Sum256([]byte(uuid.NewString()))
		return PrepareOptions{UpstreamTaskID: hex.EncodeToString(task[:]), UpstreamTurnID: hex.EncodeToString(turn[:])}, release, nil
	}
}
