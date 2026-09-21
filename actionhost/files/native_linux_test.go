package files

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/agent/claudecode"
	"github.com/chenhg5/cc-connect/core"
)

// Real native adapter, ordinary sudo login, client, Engine and file host. Only
// model output, the sandbox HTTP proxy and the external sender are substitutes.
func TestNativeFileWorkOrdinaryIdentities(t *testing.T) {
	path := os.Getenv("CC_FILE_HANDOFF_FIXTURE")
	if path == "" {
		t.Skip("requires disposable Linux identity fixture")
	}
	var f struct {
		Root, Model, EmployeeWork, NativeCLI, Client string
		GatewayUID, ModelUID                         int
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 || os.Geteuid() != f.GatewayUID || f.ModelUID == f.GatewayUID {
		t.Fatal("ordinary distinct identities required")
	}
	platform := &fileJourneyPlatform{}
	fixture := newFixture(t, platform.SendFileWithReceipt)
	configPath := filepath.Join(f.Root, "native-fixture.json")
	agent, err := claudecode.New(map[string]any{"cmd": "/usr/bin/python3 " + f.NativeCLI + " " + configPath,
		"work_dir": f.EmployeeWork, "cc_data_dir": t.TempDir(), "run_as_user": f.Model, "file_work_native": true})
	if err != nil {
		t.Fatal(err)
	}
	engine := core.NewEngine("native-fixture", agent, []core.Platform{platform}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })
	if err = engine.SetFileWorkHost(fixture.host, func(session string) (string, int, error) {
		root, err := PrepareWork(filepath.Join(f.Root, "works"), session, f.ModelUID)
		return root, f.ModelUID, err
	}); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://127.0.0.1:18743/tool" {
			http.Error(w, "unexpected target", http.StatusBadRequest)
			return
		}
		engine.ActionToolHandler().ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	raw, err = json.Marshal(map[string]any{"GatewayUID": f.GatewayUID, "ModelUID": f.ModelUID, "EmployeeWork": f.EmployeeWork,
		"Proxy": proxy.URL, "Client": f.Client, "PrivateFiles": []string{fixture.database}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, raw, 0640); err != nil {
		t.Fatal(err)
	}
	// The selected root has the model group; fixture config contains no secrets.
	info, err := os.Stat(filepath.Join(f.Root, "works"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(configPath, -1, fileGroup(info)); err != nil {
		t.Fatal(err)
	}
	key := "fixture:room:tester"
	parent := ""
	var first []fileJourneyDelivery
	for turn := 0; turn < 3; turn++ {
		id := []string{"initial", "revision", "revision-again"}[turn]
		msg := &core.Message{Platform: "fixture", SessionKey: key, ChannelID: "room", UserID: "tester", MessageID: id,
			ParentMessageID: parent, ControlledFileWork: true, Content: "Use the selected fictional files", ReplyCtx: fileJourneyRoute{"tester", id}}
		if turn == 0 {
			for _, ext := range []string{"xlsx", "docx"} {
				msg.Files = append(msg.Files, core.FileAttachment{FileName: "fictional." + ext, Data: packageBytes(t, documentParts(ext))})
			}
		}
		engine.ReceiveMessage(platform, msg)
		deadline := time.Now().Add(20 * time.Second)
		complete := false
		for time.Now().Before(deadline) {
			deliveries, replies := platform.snapshot()
			count := 0
			for _, reply := range replies {
				if strings.Contains(reply, "NATIVE_FIXTURE_DONE") {
					count++
				}
			}
			busy := false
			for _, session := range engine.GetSessions().ListSessions(key) {
				busy = busy || session.Busy()
			}
			if len(deliveries) == (turn+1)*2 && count == turn+1 && !busy {
				complete = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		deliveries, replies := platform.snapshot()
		if !complete {
			t.Fatalf("native turn %d failed: deliveries=%d replies=%v", turn, len(deliveries), replies)
		}
		for _, delivery := range deliveries {
			if delivery.route != (fileJourneyRoute{"tester", "initial"}) || validateOOXML(delivery.file.FileName, delivery.file.Data) != nil {
				t.Fatal("recipient or format changed")
			}
		}
		if turn == 0 {
			first = deliveries
			parent = "initial"
		} else {
			parent = deliveries[len(deliveries)-1].receipt
		}
		for i, old := range first {
			if !bytes.Equal(deliveries[i].file.Data, old.file.Data) {
				t.Fatal("prior delivery overwritten")
			}
		}
		if turn > 0 && bytes.Equal(deliveries[len(deliveries)-1].file.Data, first[1].file.Data) {
			t.Fatal("revision not produced")
		}
	}
}
