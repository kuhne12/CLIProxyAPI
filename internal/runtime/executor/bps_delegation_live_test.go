package executor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Explicit opt-in only. The tool returned by the real parent response is never
// executed; its synthetic message is replayed as child input in this process.
// The probe makes four upstream requests and never updates production state.
func TestBPSDelegationLive(t *testing.T) {
	path := os.Getenv("CPA_BPS_DELEGATION_PROBE_AUTH")
	if path == "" {
		t.Skip("set CPA_BPS_DELEGATION_PROBE_AUTH to an isolated credential file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read credential file")
	}
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil {
		t.Fatal("invalid credential file")
	}
	token := ojson.GetBytes(raw, "access_token").String()
	if token == "" {
		token = ojson.GetBytes(raw, "metadata.access_token").String()
	}
	if token == "" {
		t.Fatal("missing BPS token")
	}
	metadata["access_token"] = token
	auth := &coreauth.Auth{ID: "isolated-delegation-probe", Provider: "bps", Metadata: metadata}
	e := NewBPSExecutor(&config.Config{})
	id := fmt.Sprintf("delegation-probe-%d", time.Now().UnixNano())
	options := func(child, otherCaller bool) coreexecutor.Options {
		session, parent, caller := id+"-parent", "", id
		if child {
			session, parent = id+"-child", id+"-parent"
		}
		if otherCaller {
			caller += "-unproven"
		}
		return coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{
			coreexecutor.CallerScopeMetadataKey: caller, coreexecutor.CanonicalSessionIDMetadataKey: session, coreexecutor.ParentSessionIDMetadataKey: parent,
		}}
	}
	parent := ojson.NewDocString(`{"model":"gpt-6.1-sol","reasoning":{"effort":"low"},"input":"This is an isolated protocol regression. Call collaboration.spawn_agent exactly once with task_name worker and message Reply with exactly BPS_DELEGATION_PROBE_OK. Do not call any other tool.","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"task_name":{"type":"string"},"message":{"type":"string"}},"required":["task_name","message"],"additionalProperties":false}}]}]}`)
	start := time.Now()
	resp, err := e.Execute(t.Context(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: parent.Bytes()}, options(false, false))
	if err != nil {
		t.Fatalf("parent upstream request failed (type %T)", err)
	}
	output := ojson.GetBytes(resp.Payload, "output").Array()
	task, message := "", ""
	for _, item := range output {
		if item.Get("type").String() == "function_call" && item.Get("namespace").String() == "collaboration" && item.Get("name").String() == "spawn_agent" {
			if task != "" {
				t.Fatal("parent emitted multiple spawn calls")
			}
			args := ojson.Parse(item.Get("arguments").String())
			task, message = args.Get("task_name").String(), args.Get("message").String()
		}
	}
	if task != "worker" || message == "" {
		t.Fatal("parent did not emit the expected synthetic tool")
	}
	t.Logf("case=real_parent_tool terminal=completed elapsed_ms=%d", time.Since(start).Milliseconds())
	child := ojson.NewDocString(`{"model":"gpt-6.1-sol","reasoning":{"effort":"low"},"input":[{"type":"agent_message","id":"amsg_isolated_probe","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Payload:\n"},{"type":"encrypted_content","encrypted_content":""}]}]}`)
	_ = child.Set("input.0.content.1.encrypted_content", message)
	cases := []struct {
		name      string
		payload   []byte
		opts      coreexecutor.Options
		wantError bool
	}{
		{"unproven_plaintext_envelope", child.Bytes(), options(true, true), true},
		{"verified_plaintext_recovery", child.Bytes(), options(true, false), false},
		{"ordinary_text_control", []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"low"},"input":"Reply with exactly BPS_DELEGATION_PROBE_OK."}`), options(false, false), false},
	}
	for _, tc := range cases {
		start = time.Now()
		response, err := e.Execute(t.Context(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: tc.payload}, tc.opts)
		if tc.wantError {
			var failure *pluginabi.Error
			if !errors.As(err, &failure) || !failure.IsRequestScoped() || failure.HTTPStatus != 400 || failure.Retryable || failure.CredentialScoped || failure.Message != "Encrypted conversation content could not be validated; start a new delegation with valid history" {
				t.Fatalf("case=%s expected invalid_encrypted_content (error type %T)", tc.name, err)
			}
			t.Logf("case=%s error_code=invalid_encrypted_content elapsed_ms=%d", tc.name, time.Since(start).Milliseconds())
			continue
		}
		if err != nil {
			t.Fatalf("case=%s failed (error type %T)", tc.name, err)
		}
		if ojson.GetBytes(response.Payload, "status").String() != "completed" {
			t.Fatalf("case=%s did not complete", tc.name)
		}
		if !strings.Contains(string(response.Payload), "BPS_DELEGATION_PROBE_OK") {
			t.Fatalf("case=%s did not return the expected sentinel", tc.name)
		}
		t.Logf("case=%s terminal=completed sentinel_present=%t elapsed_ms=%d", tc.name, strings.Contains(string(response.Payload), "BPS_DELEGATION_PROBE_OK"), time.Since(start).Milliseconds())
	}
}
