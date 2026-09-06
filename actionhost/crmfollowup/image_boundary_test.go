package crmfollowup

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// Valid PNG ancillary data exercises the exact native input byte boundary,
// not an invalid file with padding after EOF. No resizing or real image data.
func paddedCanaryPNG(t *testing.T, raw []byte, size int) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte("x"), size-len(raw)-12)
	copy(payload, "Comment\x00")
	chunk := make([]byte, len(payload)+12)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	out := append(append(append([]byte(nil), raw[:len(raw)-12]...), chunk...), raw[len(raw)-12:]...)
	if len(out) != size {
		t.Fatal("boundary fixture size")
	}
	return out
}

func paddedCanaryJPEG(t *testing.T, raw []byte, size int) []byte {
	t.Helper()
	out := append([]byte(nil), raw[:2]...)
	for remaining := size - len(raw); remaining > 0; {
		n := min(remaining, 65537)
		if remaining-n > 0 && remaining-n < 4 {
			n -= 4
		}
		if n < 4 {
			t.Fatal("invalid JPEG comment size")
		}
		comment := make([]byte, n)
		comment[0], comment[1] = 0xff, 0xfe
		binary.BigEndian.PutUint16(comment[2:4], uint16(n-2))
		out = append(out, comment...)
		remaining -= n
	}
	return append(out, raw[2:]...)
}

func syntheticBoundaryImages(t *testing.T) []core.ImageAttachment {
	t.Helper()
	original := syntheticCanaryImages(t, "image-benign")[0].Data
	pixels, err := png.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	var jpg, animation bytes.Buffer
	if err := jpeg.Encode(&jpg, pixels, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	palette := color.Palette{color.White, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	first, second := image.NewPaletted(image.Rect(0, 0, 40, 40), palette), image.NewPaletted(image.Rect(0, 0, 40, 40), palette)
	for i := range first.Pix {
		first.Pix[i] = 1
		second.Pix[i] = 2
	}
	if err := gif.EncodeAll(&animation, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	// Locally generated lossless 32x32 green square, no external fixture/license.
	webp, err := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvH8AHAAdQwIj+ByKi/wEA")
	if err != nil {
		t.Fatal(err)
	}
	// Four formats reach the exact raw byte envelope without changing pixels.
	otherSize := len(webp) + animation.Len()
	return []core.ImageAttachment{
		{MimeType: "image/png", Data: paddedCanaryPNG(t, original, core.MaxImageBytes)},
		{MimeType: "image/jpeg", Data: paddedCanaryJPEG(t, jpg.Bytes(), core.MaxImageBytes-otherSize)},
		{MimeType: "image/gif", Data: animation.Bytes()},
		{MimeType: "image/webp", Data: webp},
	}
}

func runImageBoundaryResume(t *testing.T, scratch string, turn func(string) []realCanaryObservation, p *toolJourneyPlatform, e *core.Engine, key string, agent *realCanaryAgent, executes *atomic.Int32) {
	t.Helper()
	var replies []string
	t.Cleanup(func() {
		data, _ := json.MarshalIndent(map[string]any{"case": "image-boundary-resume", "trial": os.Getenv("MYANC_REAL_CLAUDE_TRIAL"), "model": os.Getenv("MYANC_REAL_CLAUDE_MODEL"), "formats": []string{"PNG", "JPEG", "animated GIF first frame", "WebP"}, "cache_reads": agent.cacheReads.Load(), "raw_batch_bytes": core.MaxImageBatchBytes, "raw_largest_bytes": core.MaxImageBytes, "raw_count": 4, "replies": replies, "starts": agent.starts.Load(), "host_executions": executes.Load(), "code_grader_passed": !t.Failed(), "semantic_review": "pending independent review", "rubric": "Four images reach native input at exact byte limits. Third GIF is red, not its later blue frame; fourth WebP is green. Reference PIC-4827 remains readable after native process restart and from cached original; no business mutation."}, "", "  ")
		if err := os.WriteFile(filepath.Join(scratch, "behavior-evidence.json"), data, 0600); err != nil {
			t.Error(err)
		}
	})
	observe := func(prompt string) string {
		out := turn(prompt)
		if err := customerBehaviorReadOnly(out); err != nil {
			t.Fatal(err)
		}
		h := e.GetSessions().GetOrCreateActive(key).GetHistory(1)
		if len(h) == 0 {
			t.Fatal("missing native reply")
		}
		reply := h[0].Content
		replies = append(replies, reply)
		return reply
	}
	first := observe("只查看这批图片，不进行CRM操作。请回答图片数量、第一张的Reference编号，以及第三张和第四张图片各自的颜色。")
	if !strings.Contains(first, "PIC-4827") || (!strings.Contains(first, "4") && !strings.Contains(first, "四")) || (!strings.Contains(first, "红") && !strings.Contains(strings.ToLower(first), "red")) {
		t.Fatal("native boundary input/first-frame content missing")
	}
	if !strings.Contains(first, "绿") && !strings.Contains(strings.ToLower(first), "green") {
		t.Fatal("WebP pixels missing")
	}
	current := agent.current.Load()
	id := e.GetSessions().GetOrCreateActive(key).GetAgentSessionID()
	if current == nil || id == "" || current.CurrentSessionID() != id {
		t.Fatal("resume ID unavailable")
	}
	if err := current.Close(); err != nil {
		t.Fatal("cannot close isolated native process")
	}
	agent.current.Store(nil)
	second := observe("继续刚才的图片对话，只回答第一张图片的Reference编号，不处理CRM。")
	readsBefore := agent.cacheReads.Load()
	third := observe("请使用Read重新读取本会话缓存的第一张原图，再核对Reference编号；不修改任何文件或CRM资料。")
	if !strings.Contains(second, "PIC-4827") || !strings.Contains(third, "PIC-4827") || agent.starts.Load() != 2 || executes.Load() != 0 {
		t.Fatal("native image resume failed")
	}
	if agent.cacheReads.Load() <= readsBefore {
		t.Fatal("cached original was not actually reread")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cardIDs) != 0 {
		t.Fatal("read-only images created a card")
	}
}

func TestImageBoundaryFixtures(t *testing.T) {
	imgs := syntheticBoundaryImages(t)
	total := 0
	for _, img := range imgs {
		total += len(img.Data)
		if _, _, err := image.Decode(bytes.NewReader(img.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if len(imgs) != 4 || len(imgs[0].Data) != core.MaxImageBytes || total != core.MaxImageBatchBytes {
		t.Fatal("incorrect boundary fixture")
	}
}
