package helps

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func delegationOpts(caller, session, parent string) coreexecutor.Options {
	return coreexecutor.Options{Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: caller, coreexecutor.CanonicalSessionIDMetadataKey: session, coreexecutor.ParentSessionIDMetadataKey: parent}}
}
func delegationCall(name, target, message string) []byte {
	args := ojson.NewDocString(`{}`)
	field := "target"
	if name == "spawn_agent" {
		field = "task_name"
	}
	_ = args.Set(field, target)
	_ = args.Set("message", message)
	doc := ojson.NewDocString(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","call_id":"call_fixture","status":"completed"}}`)
	_ = doc.Set("item.name", name)
	_ = doc.Set("item.arguments", args.String())
	return doc.Bytes()
}
func delegationInput(author, recipient, message string) *ojson.Doc {
	doc := ojson.NewDocString(`{"input":[{"type":"agent_message","id":"amsg_test","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Payload:"},{"type":"encrypted_content","encrypted_content":"task"}]},{"type":"reasoning","encrypted_content":"opaque-reasoning"},{"type":"compaction","encrypted_content":"opaque-compaction"}]}`)
	_ = doc.Set("input.0.author", author)
	_ = doc.Set("input.0.recipient", recipient)
	_ = doc.Set("input.0.content.1.encrypted_content", message)
	return doc
}
func TestBPSDelegationKnownPlaintextOnly(t *testing.T) {
	store := newBPSDelegationProofStore(time.Now)
	root := delegationOpts("caller", "root-session", "")
	child := delegationOpts("caller", "child-session", "root-session")
	task := "Inspect the sample without changing files."
	if n := store.remember(delegationCall("spawn_agent", "worker", task), root); n != 1 {
		t.Fatalf("recorded %d", n)
	}
	doc := delegationInput("/root", "/root/worker", task)
	if n := store.normalize(doc, child); n != 1 {
		t.Fatalf("normalized %d", n)
	}
	if doc.Get("input.0.content.1.type").String() != "input_text" || doc.Get("input.0.content.1.text").String() != task || doc.Get("input.0.content.1.encrypted_content").Exists() {
		t.Fatal("incorrect targeted conversion")
	}
	if doc.Get("input.0.type").String() != "agent_message" || doc.Get("input.0.author").String() != "/root" || doc.Get("input.1.encrypted_content").String() != "opaque-reasoning" || doc.Get("input.2.encrypted_content").String() != "opaque-compaction" {
		t.Fatal("unrelated protocol state changed")
	}
	if n := store.normalize(doc, child); n != 0 {
		t.Fatal("not idempotent")
	}
	unknown := delegationInput("/root", "/root/worker", "opaque-or-unknown-plaintext")
	before := unknown.String()
	if n := store.normalize(unknown, child); n != 0 || unknown.String() != before {
		t.Fatal("unknown content changed")
	}
}
func TestBPSDelegationScopeIsolation(t *testing.T) {
	for _, tc := range []struct{ name, caller, parent, author, recipient, message string }{
		{"caller", "other", "root", "/root", "/root/worker", "task"},
		{"parent", "caller", "other", "/root", "/root/worker", "task"},
		{"author", "caller", "root", "/root/other", "/root/worker", "task"},
		{"recipient", "caller", "root", "/root", "/root/other", "task"},
		{"message", "caller", "root", "/root", "/root/worker", "changed"},
		{"empty caller", "", "root", "/root", "/root/worker", "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newBPSDelegationProofStore(time.Now)
			store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("caller", "root", ""))
			doc := delegationInput(tc.author, tc.recipient, tc.message)
			before := doc.String()
			if store.normalize(doc, delegationOpts(tc.caller, "child", tc.parent)) != 0 || doc.String() != before {
				t.Fatal("cross-scope rewrite")
			}
		})
	}
}
func TestBPSDelegationNestedAndSiblingMessages(t *testing.T) {
	store := newBPSDelegationProofStore(time.Now)
	root := delegationOpts("caller", "root", "")
	a := delegationOpts("caller", "a", "root")
	b := delegationOpts("caller", "b", "root")
	leaf := delegationOpts("caller", "leaf", "a")
	for _, target := range []string{"a", "b"} {
		store.remember(delegationCall("spawn_agent", target, "initial"), root)
		opts := a
		if target == "b" {
			opts = b
		}
		if store.normalize(delegationInput("/root", "/root/"+target, "initial"), opts) != 1 {
			t.Fatal("cannot bind child")
		}
	}
	store.remember(delegationCall("spawn_agent", "leaf", "nested"), a)
	if store.normalize(delegationInput("/root/a", "/root/a/leaf", "nested"), leaf) != 1 {
		t.Fatal("nested child")
	}
	store.remember(delegationCall("send_message", "/root/b", "sibling"), a)
	if store.normalize(delegationInput("/root/a", "/root/b", "sibling"), b) != 1 {
		t.Fatal("sibling delivery")
	}
	store.remember(delegationCall("followup_task", "a", "followup"), root)
	if store.normalize(delegationInput("/root", "/root/a", "followup"), a) != 1 {
		t.Fatal("followup delivery")
	}
	store.remember(delegationCall("send_message", "/root", "reply"), leaf)
	if store.normalize(delegationInput("/root/a/leaf", "/root", "reply"), root) != 1 {
		t.Fatal("reply delivery")
	}
}
func TestBPSDelegationRejectsUnvalidatedAndAmbiguousOutput(t *testing.T) {
	for _, field := range []string{"type", "item.namespace", "item.status", "item.type", "item.name"} {
		t.Run(field, func(t *testing.T) {
			store := newBPSDelegationProofStore(time.Now)
			doc := ojson.NewDoc(delegationCall("spawn_agent", "worker", "task"))
			_ = doc.Set(field, "untrusted")
			if store.remember(doc.Bytes(), delegationOpts("caller", "root", "")) != 0 {
				t.Fatal("untrusted output recorded")
			}
		})
	}
	store := newBPSDelegationProofStore(time.Now)
	for _, task := range []string{"../worker", "a/b", "", "/root/worker"} {
		if store.remember(delegationCall("spawn_agent", task, "task"), delegationOpts("caller", "root", "")) != 0 {
			t.Fatal("invalid task name recorded")
		}
	}
	if store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("caller", "child", "unknown-parent")) != 0 {
		t.Fatal("unproven child author recorded")
	}
}
func TestBPSDelegationExpiryAndCapacity(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	store := newBPSDelegationProofStore(func() time.Time { return now })
	opts := delegationOpts("caller", "root", "")
	store.remember(delegationCall("spawn_agent", "worker", "task"), opts)
	now = now.Add(bpsDelegationTTL)
	if store.normalize(delegationInput("/root", "/root/worker", "task"), delegationOpts("caller", "child", "root")) != 0 {
		t.Fatal("expired proof accepted")
	}
	for i := 0; i < bpsDelegationMaxProofs+10; i++ {
		store.remember(delegationCall("spawn_agent", fmt.Sprintf("w%d", i), "task"), opts)
	}
	if len(store.proofs) > bpsDelegationMaxProofs || len(store.sessions) > bpsDelegationMaxSessions {
		t.Fatal("store unbounded")
	}
}
func TestBPSDelegationConcurrentAccess(t *testing.T) {
	store := newBPSDelegationProofStore(time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			caller := fmt.Sprint(i)
			root := delegationOpts(caller, "root", "")
			child := delegationOpts(caller, "child", "root")
			for j := 0; j < 10; j++ {
				store.remember(delegationCall("spawn_agent", "worker", "task"), root)
				store.normalize(delegationInput("/root", "/root/worker", "task"), child)
			}
		}(i)
	}
	wg.Wait()
}

func TestBPSDelegationRejectsAmbiguousInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ojson.Doc)
	}{
		{"duplicate input", func(d *ojson.Doc) {
			_ = d.SetRaw("input.0.content.1", `{"type":"encrypted_content","type":"input_text","encrypted_content":"task"}`)
		}},
		{"duplicate author", func(d *ojson.Doc) {
			_ = d.SetRaw("input.0", `{"type":"agent_message","author":"/root","author":"/root/other","recipient":"/root/worker","content":[{"type":"encrypted_content","encrypted_content":"task"}]}`)
		}},
		{"text conflict", func(d *ojson.Doc) { _ = d.Set("input.0.content.1.text", "conflicting") }},
		{"nonstring", func(d *ojson.Doc) { _ = d.Set("input.0.content.1.encrypted_content", 42) }},
		{"not agent message", func(d *ojson.Doc) { _ = d.Set("input.0.type", "message") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newBPSDelegationProofStore(time.Now)
			store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("caller", "root", ""))
			doc := delegationInput("/root", "/root/worker", "task")
			tc.mutate(doc)
			before := doc.String()
			if store.normalize(doc, delegationOpts("caller", "child", "root")) != 0 || doc.String() != before {
				t.Fatal("ambiguous input changed")
			}
		})
	}
	store := newBPSDelegationProofStore(time.Now)
	root := delegationOpts("caller", "root", "")
	for _, name := range []string{"worker", "other"} {
		store.remember(delegationCall("spawn_agent", name, "task"), root)
	}
	doc := delegationInput("/root", "/root/worker", "task")
	other := delegationInput("/root", "/root/other", "task")
	_ = doc.SetRaw("input.-1", other.Get("input.0").Raw)
	before := doc.String()
	if store.normalize(doc, delegationOpts("caller", "child", "root")) != 0 || doc.String() != before {
		t.Fatal("conflicting bootstrap bound a child")
	}
}

func TestBPSDelegationStrictOutputArguments(t *testing.T) {
	for _, args := range []string{`{"task_name":"worker","message":"task","message":"other"}`, `{"task_name":"worker","message":null}`, `[]`, `not-json`} {
		store := newBPSDelegationProofStore(time.Now)
		doc := ojson.NewDoc(delegationCall("spawn_agent", "worker", "task"))
		_ = doc.Set("item.arguments", args)
		if store.remember(doc.Bytes(), delegationOpts("caller", "root", "")) != 0 {
			t.Fatal("invalid arguments remembered")
		}
	}
	store := newBPSDelegationProofStore(time.Now)
	doc := ojson.NewDoc(delegationCall("spawn_agent", "worker", "task"))
	_ = doc.Set("item.name", "collaboration.spawn_agent")
	_ = doc.Delete("item.namespace")
	if store.remember(doc.Bytes(), delegationOpts("caller", "root", "")) != 1 {
		t.Fatal("flattened function name unsupported")
	}
}

func TestBPSDelegationMetadataCannotRebindSession(t *testing.T) {
	store := newBPSDelegationProofStore(time.Now)
	root := delegationOpts("caller", "root", "")
	child := delegationOpts("caller", "child", "root")
	store.remember(delegationCall("spawn_agent", "worker", "task"), root)
	if store.normalize(delegationInput("/root", "/root/worker", "task"), child) != 1 {
		t.Fatal("bootstrap failed")
	}
	for _, parent := range []string{"", "other", "child"} {
		opts := delegationOpts("caller", "child", parent)
		if store.normalize(delegationInput("/root", "/root/worker", "task"), opts) != 0 {
			t.Fatal("session rebound")
		}
		if store.remember(delegationCall("spawn_agent", "nested", "task"), opts) != 0 {
			t.Fatal("rebound author accepted")
		}
	}
	for _, opts := range []coreexecutor.Options{
		{}, delegationOpts("", "root", ""), delegationOpts("caller", "", ""), delegationOpts("caller\n", "root", ""), delegationOpts("caller", "root", " root"),
	} {
		if store.remember(delegationCall("spawn_agent", "worker", "task"), opts) != 0 {
			t.Fatal("invalid identity accepted")
		}
	}
	fresh := newBPSDelegationProofStore(time.Now)
	if fresh.normalize(delegationInput("/root", "/root/worker", "task"), child) != 0 {
		t.Fatal("restart guessed provenance")
	}
}

func TestBPSDelegationSessionCapacityAndDuplicateExpiry(t *testing.T) {
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	store := newBPSDelegationProofStore(func() time.Time { return now })
	for i := 0; i < bpsDelegationMaxSessions; i++ {
		if store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts(fmt.Sprint(i), "root", "")) != 1 {
			t.Fatal("capacity exhausted early")
		}
	}
	if len(store.sessions) != bpsDelegationMaxSessions {
		t.Fatal("incorrect bound")
	}
	if store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("overflow", "root", "")) != 0 {
		t.Fatal("capacity exceeded")
	}
	doc := delegationInput("/root", "/root/worker", "task")
	before := doc.String()
	if store.normalize(doc, delegationOpts("0", "child", "root")) != 0 || doc.String() != before {
		t.Fatal("full session store did not fail closed")
	}
	now = now.Add(bpsDelegationTTL / 2)
	store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("0", "root", ""))
	now = now.Add(bpsDelegationTTL / 2)
	if store.normalize(doc, delegationOpts("0", "child", "root")) != 0 {
		t.Fatal("duplicate event extended TTL")
	}
	if store.remember(delegationCall("spawn_agent", "worker", "task"), delegationOpts("recovered", "root", "")) != 1 {
		t.Fatal("expired capacity not reclaimed")
	}
}
