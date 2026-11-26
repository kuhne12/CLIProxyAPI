package helps

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestRestoreCodexCheckpointTools(t *testing.T) {
	const exec = `{"type":"additional_tools","role":"developer","id":"at_exec","tools":[{"type":"custom","name":"exec"}]}`
	const edit = `{"type":"additional_tools","role":"developer","id":"at_edit","tools":[{"type":"function","name":"edit"}]}`
	const checkpoint = `{"type":"compaction","id":"cmp_a","encrypted_content":"state"}`
	const user = `{"role":"user","content":"continue"}`
	docWith := func(items ...string) *ojson.Doc {
		return ojson.NewDocString(`{"input":[` + strings.Join(items, ",") + `]}`)
	}
	doc := docWith(exec, checkpoint, strings.Replace(edit, `"role":"developer",`, "", 1), checkpoint, exec, user)
	RestoreCodexCheckpointTools(doc)
	items := doc.Get("input").Array()
	if len(items) != 7 || items[5].Get("tools.0.name").String() != "edit" || items[5].Get("id").Exists() || items[6].Raw != user {
		t.Fatalf("incorrect order or duplicate historical ID: %s", doc.String())
	}
	before := doc.String()
	RestoreCodexCheckpointTools(doc)
	if doc.String() != before {
		t.Fatal("repeated preparation duplicated the catalog")
	}
	for name, doc := range map[string]*ojson.Doc{
		"no checkpoint":         docWith(exec, user),
		"no declarations":       docWith(user, checkpoint, user),
		"tools already visible": docWith(checkpoint, exec, user),
		"untrusted role":        docWith(strings.Replace(exec, "developer", "user", 1), checkpoint, user),
		"incomplete checkpoint": docWith(exec, strings.Replace(checkpoint, `"id":`, `"status":"in_progress","id":`, 1), user),
		"incremental":           ojson.NewDocString(`{"previous_response_id":"resp","input":[` + exec + "," + checkpoint + `]}`),
		"tools disabled":        ojson.NewDocString(`{"tool_choice":"none","input":[` + exec + "," + checkpoint + `]}`),
	} {
		t.Run(name, func(t *testing.T) {
			before := doc.String()
			RestoreCodexCheckpointTools(doc)
			if doc.String() != before {
				t.Fatalf("unexpected tool update: %s", doc.String())
			}
		})
	}
}
