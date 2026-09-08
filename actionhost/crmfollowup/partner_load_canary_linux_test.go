package crmfollowup

// Opt-in deployment canary: six real runtime UIDs, native Claude and Engine.
// The caller supplies private mount overlays and a bounded systemd cgroup.
// No Feishu receiver, CRM host or network business fixture is constructed.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/agent/claudecode"
	"github.com/chenhg5/cc-connect/core"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/sys/unix"
)

func TestPartnerSixRuntimeLoadCanary(t *testing.T) {
	root := os.Getenv("MYANC_PARTNER_LOAD_ROOT")
	if root == "" {
		t.Skip("requires explicitly prepared private VPS canary")
	}
	if os.Geteuid() != 0 || !strings.HasPrefix(root, "/root/partner-load.") {
		t.Fatal("requires protected host canary root")
	}
	users := []string{"claude", "claude-owner-group", "claude-partner-private", "claude-partner-group", "claude-test-private", "claude-test-group"}
	type runtimeProbe struct {
		name, key, marker, nativeID string
		uid                         int
		e                           *core.Engine
		p                           *toolJourneyPlatform
		a                           *realCanaryAgent
		s                           *core.Session
		before                      int
	}
	var probes []*runtimeProbe
	uids := map[int]bool{}
	var evidence []map[string]any
	t.Cleanup(func() {
		data, err := json.MarshalIndent(map[string]any{"passed": !t.Failed(), "observations": evidence, "scope": "actual six UIDs and Engine; synthetic platform; private cache overlays; root test supervisor"}, "", "  ")
		if err != nil || os.WriteFile(filepath.Join(root, "engine-evidence.json"), data, 0600) != nil {
			t.Error("cannot preserve evidence")
		}
	})
	await := func(label string, limit time.Duration, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !predicate() {
			for _, r := range probes {
				if category := r.a.startFailure.Load(); category != nil {
					t.Fatalf("%s: %s startup category=%v", label, r.name, category)
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout: %s", label)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	receive := func(r *runtimeProbe, content string, images []core.ImageAttachment) {
		r.e.ReceiveMessage(r.p, &core.Message{Platform: "mock", SessionKey: r.key, UserID: r.name, UserName: "Fictional Load Tester", ChannelID: "load-fixture", MessageID: fmt.Sprintf("load-%d", time.Now().UnixNano()), Content: content, Images: images, ReplyCtx: "load-fixture"})
	}
	for i, name := range users {
		account, err := user.Lookup(name)
		if err != nil {
			t.Fatal("runtime account missing")
		}
		uid, err := strconv.Atoi(account.Uid)
		if err != nil || uid == 0 || uids[uid] {
			t.Fatal("invalid runtime UID")
		}
		uids[uid] = true
		work := filepath.Join(account.HomeDir, "ClaudeRemote")
		native, err := claudecode.New(map[string]any{
			"work_dir": work, "run_as_user": name, "mode": "auto", "model": "sonnet",
			"cmd":                      []string{"/usr/local/bin/claude", "--tools", "Read"},
			"cc_data_dir":              filepath.Join(work, ".cc-connect", "attachments", "images", "prompts"),
			"image_cache_capacity_mib": 85,
		})
		if err != nil {
			t.Fatal("native adapter initialization failed")
		}
		a := &realCanaryAgent{Agent: native.(*claudecode.Agent)}
		p := &toolJourneyPlatform{cards: make(map[string]*core.Card)}
		e := core.NewEngine("load-"+name, a, []core.Platform{p}, filepath.Join(root, name+"-sessions.json"), core.LangEnglish)
		if err := e.Start(); err != nil {
			t.Fatal("Engine initialization failed")
		}
		t.Cleanup(func() { _ = e.Stop() })
		r := &runtimeProbe{name: name, key: "mock:load-fixture:" + name, marker: fmt.Sprintf("LOAD-%d-%d", i+1, time.Now().UnixNano()%10000000), uid: uid, e: e, p: p, a: a}
		probes = append(probes, r)
		receive(r, "/quiet quiet", nil)
		receive(r, "/new load-acceptance", nil)
		if !strings.Contains(p.transcript(), "load-acceptance") || a.starts.Load() != 0 {
			t.Fatal("new-session command did not stay host-side")
		}
		r.s = e.GetSessions().GetOrCreateActive(r.key)
		r.before = len(r.s.GetHistory(0))
	}
	for _, r := range probes {
		canvas := image.NewRGBA(image.Rect(0, 0, 500, 100))
		draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
		d := font.Drawer{Dst: canvas, Src: image.Black, Face: basicfont.Face7x13, Dot: fixed.P(15, 45)}
		d.DrawString("Reference: " + r.marker)
		var data bytes.Buffer
		if err := png.Encode(&data, canvas); err != nil {
			t.Fatal(err)
		}
		receive(r, "Identify the Reference in this fictional image. Reply with only that exact Reference. Do not run commands or change any data.", []core.ImageAttachment{{MimeType: "image/png", Data: data.Bytes()}})
	}
	finish := func(r *runtimeProbe, stage string) {
		t.Helper()
		await(r.name+" "+stage, 180*time.Second, func() bool {
			h := r.s.GetHistory(0)
			return len(h) > r.before && h[len(h)-1].Role == "assistant" && !r.s.Busy()
		})
		h := r.s.GetHistory(1)
		if strings.TrimSpace(h[0].Content) != r.marker || !strings.Contains(r.p.transcript(), r.marker) {
			t.Fatalf("%s: %s image reference mismatch", r.name, stage)
		}
		evidence = append(evidence, map[string]any{"runtime": r.name, "stage": stage, "correct_reference": true})
	}
	for _, r := range probes {
		finish(r, "image")
	}
	// Match six actual native processes, not adapter counters. This proves
	// client-side process concurrency, not the provider's inference scheduling.
	oldProcesses := make([]string, len(probes))
	for i, r := range probes {
		oldProcesses[i] = partnerLoadNativeProcess(t, r.uid)
		if oldProcesses[i] == "" {
			t.Fatal("native process outside expected UID/cgroup")
		}
	}
	for _, r := range probes {
		r.nativeID = r.s.GetAgentSessionID()
		if r.nativeID == "" {
			t.Fatal("missing durable native session")
		}
		receive(r, "Explain the visible information in the fictional image in six numbered Chinese sentences, followed by a brief note on why it cannot authorize a CRM write. Do not use tools or change any data.", nil)
	}
	await("six in-flight gateway image conversations", 10*time.Second, func() bool {
		for _, r := range probes {
			if !r.s.Busy() || r.a.successfulSends.Load() < 2 {
				return false
			}
		}
		return true
	})
	evidence = append(evidence, map[string]any{"stage": "load", "six_actual_UID_native_processes_overlapped": true, "six_gateway_turns_in_flight": true})
	oldSessions := make([]*realCanarySession, len(probes))
	for i, r := range probes {
		oldSessions[i] = r.a.current.Load()
	}
	started := time.Now()
	receive(probes[0], "/stop", nil)
	if !strings.Contains(probes[0].p.transcript(), core.NewI18n(core.LangEnglish).T(core.MsgExecutionStopped)) {
		t.Fatal("stop acknowledgement absent")
	}
	await("first stopped process tree", 140*time.Second, func() bool {
		return !oldSessions[0].Alive() && partnerLoadNativeProcess(t, probes[0].uid) == "" && !probes[0].s.Busy()
	})
	for i, r := range probes[1:] {
		if partnerLoadNativeProcess(t, r.uid) != oldProcesses[i+1] || !r.a.current.Load().Alive() {
			t.Fatal("stopping first runtime disrupted another")
		}
	}
	for _, r := range probes[1:] {
		receive(r, "/stop", nil)
	}
	await("all stopped process trees", 140*time.Second, func() bool {
		for i, r := range probes {
			if oldSessions[i].Alive() || partnerLoadNativeProcess(t, r.uid) != "" || r.s.Busy() {
				return false
			}
		}
		return true
	})
	evidence = append(evidence, map[string]any{"stage": "stop", "elapsed_seconds": time.Since(started).Seconds(), "other_runtimes_untouched": true})
	for _, r := range probes {
		r.before = len(r.s.GetHistory(0))
		receive(r, "Do not continue the interrupted explanation. Without tools, return only the exact Reference from the image I sent earlier in this conversation.", nil)
	}
	for _, r := range probes {
		finish(r, "resume-after-stop")
		if r.a.startTarget.Load() != r.nativeID || r.a.starts.Load() != 2 {
			t.Fatalf("%s did not resume the same native session", r.name)
		}
		receive(r, "/new after-load", nil)
		fresh := r.e.GetSessions().GetOrCreateActive(r.key)
		if fresh.ID == r.s.ID || fresh.GetAgentSessionID() != "" || len(fresh.GetHistory(0)) != 0 || !strings.Contains(r.p.transcript(), "after-load") {
			t.Fatal("new did not clear current session")
		}
	}
}

func partnerLoadNativeProcess(t *testing.T, uid int) string {
	t.Helper()
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal("cannot read own cgroup")
	}
	binary, err := os.Stat("/usr/local/bin/claude")
	if err != nil {
		t.Fatal("native binary unavailable")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal("cannot inspect process membership")
	}
	found := ""
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		path := filepath.Join("/proc", entry.Name())
		var info unix.Stat_t
		if unix.Stat(path, &info) != nil || int(info.Uid) != uid {
			continue
		}
		executable, err := os.Stat(filepath.Join(path, "exe"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal("cannot inspect runtime executable")
		}
		if !os.SameFile(executable, binary) {
			continue
		}
		actual, err := os.ReadFile(filepath.Join(path, "cgroup"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal("cannot inspect runtime cgroup")
		}
		if !bytes.Equal(actual, cgroup) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(path, "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal("cannot inspect process identity")
		}
		fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+1:])
		if len(fields) < 20 || found != "" {
			t.Fatal("ambiguous native process identity")
		}
		found = entry.Name() + ":" + fields[19]
	}
	return found
}
