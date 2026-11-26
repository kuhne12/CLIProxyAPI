package helps

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	bpsDelegationTTL         = 24 * time.Hour
	bpsDelegationMaxProofs   = 4096
	bpsDelegationMaxSessions = 1024
	bpsDelegationMaxMessage  = 256 << 10
)

type bpsDelegationKey struct {
	family            [32]byte
	author, recipient string
	digest            [32]byte
}
type bpsDelegationProof struct {
	expires     time.Time
	spawnParent [32]byte
}
type bpsDelegationSession struct {
	family, parent [32]byte
	agent          string
	expires        time.Time
}
type bpsDelegationScope struct {
	session, parent [32]byte
	child           bool
}

// The native BPS and Codex executors share this host-owned, bounded store. It
// deliberately retains no message text and does not import untrusted history.
// Restart or expiry loses provenance, so unknown opaque content stays opaque.
type bpsDelegationProofStore struct {
	mu       sync.Mutex
	now      func() time.Time
	proofs   map[bpsDelegationKey]bpsDelegationProof
	sessions map[[32]byte]bpsDelegationSession
}

var bpsDelegationProofs = newBPSDelegationProofStore(time.Now)

func newBPSDelegationProofStore(now func() time.Time) *bpsDelegationProofStore {
	return &bpsDelegationProofStore{now: now, proofs: make(map[bpsDelegationKey]bpsDelegationProof), sessions: make(map[[32]byte]bpsDelegationSession)}
}

func bpsDelegationIdentity(opts coreexecutor.Options) (bpsDelegationScope, bool) {
	caller, _ := opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	session, _ := opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey].(string)
	parent, _ := opts.Metadata[coreexecutor.ParentSessionIDMetadataKey].(string)
	valid := func(v string) bool {
		return v != "" && len(v) <= 1024 && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
	}
	if !valid(caller) || !valid(session) || (parent != "" && !valid(parent)) || parent == session {
		return bpsDelegationScope{}, false
	}
	scope := bpsDelegationScope{session: sha256.Sum256([]byte(caller + "\x00" + session)), child: parent != ""}
	if scope.child {
		scope.parent = sha256.Sum256([]byte(caller + "\x00" + parent))
	}
	return scope, true
}
func bpsDelegationPath(value string) bool {
	if value == "/root" {
		return true
	}
	if !strings.HasPrefix(value, "/root/") || len(value) > 256 {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "/root/"), "/")
	if len(parts) > 16 {
		return false
	}
	for _, part := range parts {
		if !bpsDelegationName(part) {
			return false
		}
	}
	return true
}
func bpsDelegationName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func bpsDelegationUnique(value ojson.Result) bool {
	if !value.IsObject() {
		return false
	}
	seen := make(map[string]bool)
	unique := true
	value.ForEach(func(k, v ojson.Result) bool {
		if seen[k.String()] {
			unique = false
			return false
		}
		seen[k.String()] = true
		return true
	})
	return unique
}
func (s *bpsDelegationProofStore) prune(now time.Time) {
	if len(s.proofs) >= bpsDelegationMaxProofs {
		for key, proof := range s.proofs {
			if !now.Before(proof.expires) {
				delete(s.proofs, key)
			}
		}
	}
	if len(s.sessions) >= bpsDelegationMaxSessions {
		for key, entry := range s.sessions {
			if !now.Before(entry.expires) {
				delete(s.sessions, key)
			}
		}
	}
}
func (s *bpsDelegationProofStore) session(key [32]byte, now time.Time) (bpsDelegationSession, bool) {
	value, ok := s.sessions[key]
	if ok && !now.Before(value.expires) {
		delete(s.sessions, key)
		ok = false
	}
	return value, ok
}

// RememberBPSDelegationOutput accepts only already validated, client-facing BPS
// tool output at the executor handoff boundary, never raw provider events or
// client-supplied history. Registration precedes dispatch so an immediate child
// request cannot race its source proof. This is not a downstream delivery ACK.
func RememberBPSDelegationOutput(data []byte, opts coreexecutor.Options) {
	bpsDelegationProofs.remember(data, opts)
}
func (s *bpsDelegationProofStore) remember(data []byte, opts coreexecutor.Options) int {
	scope, ok := bpsDelegationIdentity(opts)
	if !ok || !json.Valid(data) {
		return 0
	}
	event := ojson.ParseBytes(data)
	if !bpsDelegationUnique(event) {
		return 0
	}
	var items []ojson.Result
	switch event.Get("type").String() {
	case "response.output_item.done":
		items = []ojson.Result{event.Get("item")}
	case "response.completed":
		response := event.Get("response")
		if !bpsDelegationUnique(response) || response.Get("status").String() != "completed" {
			return 0
		}
		items = response.Get("output").Array()
	default:
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	source, known := s.session(scope.session, now)
	if known && source.parent != scope.parent {
		return 0
	}
	if !known {
		if scope.child || len(s.sessions) >= bpsDelegationMaxSessions {
			return 0
		}
		source = bpsDelegationSession{family: scope.session, agent: "/root", expires: now.Add(bpsDelegationTTL)}
	}
	count := 0
	for _, item := range items {
		if !bpsDelegationUnique(item) || item.Get("type").String() != "function_call" || item.Get("status").String() != "completed" || item.Get("call_id").Type != ojson.String || item.Get("call_id").String() == "" {
			continue
		}
		name, namespace := item.Get("name").String(), item.Get("namespace").String()
		if namespace == "" && strings.HasPrefix(name, "collaboration.") {
			name = strings.TrimPrefix(name, "collaboration.")
			namespace = "collaboration"
		}
		if namespace != "collaboration" {
			continue
		}
		arguments := item.Get("arguments")
		if arguments.Type != ojson.String || !json.Valid([]byte(arguments.String())) {
			continue
		}
		args := ojson.Parse(arguments.String())
		if !bpsDelegationUnique(args) {
			continue
		}
		message := args.Get("message")
		if message.Type != ojson.String || message.String() == "" || len(message.String()) > bpsDelegationMaxMessage {
			continue
		}
		target := ""
		spawnParent := [32]byte{}
		switch name {
		case "spawn_agent":
			task := args.Get("task_name")
			if task.Type != ojson.String || !bpsDelegationName(task.String()) {
				continue
			}
			target = source.agent + "/" + task.String()
			spawnParent = scope.session
		case "send_message", "followup_task":
			value := args.Get("target")
			if value.Type != ojson.String {
				continue
			}
			target = value.String()
			if !strings.HasPrefix(target, "/") {
				target = source.agent + "/" + target
			}
		default:
			continue
		}
		if !bpsDelegationPath(target) {
			continue
		}
		key := bpsDelegationKey{source.family, source.agent, target, sha256.Sum256([]byte(message.String()))}
		if prior, exists := s.proofs[key]; exists && now.Before(prior.expires) {
			count++
			continue
		}
		if len(s.proofs) >= bpsDelegationMaxProofs {
			continue
		}
		s.proofs[key] = bpsDelegationProof{expires: now.Add(bpsDelegationTTL), spawnParent: spawnParent}
		count++
	}
	if count > 0 {
		s.sessions[scope.session] = source
	}
	return count
}

// NormalizeBPSDelegationInputDoc repairs only known BPS plaintext that a client
// placed in an agent_message encrypted_content part. Call before payload rules.
// Genuine/unknown opaque state, reasoning, compaction and other messages survive.
func NormalizeBPSDelegationInputDoc(doc *ojson.Doc, opts coreexecutor.Options) int {
	count := bpsDelegationProofs.normalize(doc, opts)
	if count > 0 {
		log.WithField("normalized_parts", count).Debug("restored verified BPS delegation plaintext")
	}
	return count
}
func (s *bpsDelegationProofStore) normalize(doc *ojson.Doc, opts coreexecutor.Options) int {
	scope, ok := bpsDelegationIdentity(opts)
	if !ok || doc == nil || !bpsDelegationUnique(doc.Get("@this")) || !doc.Get("input").IsArray() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	target, known := s.session(scope.session, now)
	if known && target.parent != scope.parent {
		return 0
	}
	input := doc.Get("input").Array()
	if !known {
		if !scope.child || len(s.sessions) >= bpsDelegationMaxSessions {
			return 0
		}
		parent, found := s.session(scope.parent, now)
		if !found {
			return 0
		}
		// Bootstrap a child only with an exact spawn proof from its known parent.
		for _, item := range input {
			author, recipient, parts, valid := bpsDelegationMessage(item)
			if !valid || author != parent.agent {
				continue
			}
			for _, part := range parts {
				message, validPart := bpsDelegationEncryptedPart(part)
				if !validPart {
					continue
				}
				proof, exists := s.proofs[bpsDelegationKey{parent.family, author, recipient, sha256.Sum256([]byte(message))}]
				if !exists || !now.Before(proof.expires) || proof.spawnParent != scope.parent {
					continue
				}
				if known && target.agent != recipient {
					return 0
				}
				target = bpsDelegationSession{family: parent.family, parent: scope.parent, agent: recipient, expires: parent.expires}
				if proof.expires.Before(target.expires) {
					target.expires = proof.expires
				}
				known = true
			}
		}
		if !known {
			return 0
		}
	}
	count := 0
	for i, item := range input {
		author, recipient, parts, valid := bpsDelegationMessage(item)
		if !valid || recipient != target.agent {
			continue
		}
		for j, part := range parts {
			message, validPart := bpsDelegationEncryptedPart(part)
			if !validPart {
				continue
			}
			proof, exists := s.proofs[bpsDelegationKey{target.family, author, recipient, sha256.Sum256([]byte(message))}]
			if !exists || !now.Before(proof.expires) {
				continue
			}
			replacement := ojson.NewDocString(part.Raw)
			if replacement.Set("type", "input_text") != nil || replacement.Set("text", message) != nil || replacement.Delete("encrypted_content") != nil {
				continue
			}
			if doc.SetRaw(fmt.Sprintf("input.%d.content.%d", i, j), replacement.String()) != nil {
				continue
			}
			count++
		}
	}
	if count > 0 {
		s.sessions[scope.session] = target
	}
	return count
}
func bpsDelegationMessage(item ojson.Result) (string, string, []ojson.Result, bool) {
	if !bpsDelegationUnique(item) || item.Get("type").String() != "agent_message" || item.Get("author").Type != ojson.String || item.Get("recipient").Type != ojson.String || !item.Get("content").IsArray() {
		return "", "", nil, false
	}
	author, recipient := item.Get("author").String(), item.Get("recipient").String()
	return author, recipient, item.Get("content").Array(), bpsDelegationPath(author) && bpsDelegationPath(recipient)
}
func bpsDelegationEncryptedPart(part ojson.Result) (string, bool) {
	if !bpsDelegationUnique(part) || part.Get("type").String() != "encrypted_content" || part.Get("text").Exists() {
		return "", false
	}
	value := part.Get("encrypted_content")
	return value.String(), value.Type == ojson.String && len(value.String()) > 0 && len(value.String()) <= bpsDelegationMaxMessage
}
