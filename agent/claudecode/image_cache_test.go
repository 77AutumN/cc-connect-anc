package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newTestImageCache(t *testing.T, capacity int64, now *time.Time) *imageCache {
	t.Helper()
	return &imageCache{dir: filepath.Join(t.TempDir(), "images"), active: map[string]int{}, capacity: capacity, now: func() time.Time { return *now }, freeSpace: func(string) (uint64, error) { return 10 << 30, nil }}
}

func TestImageCacheStartupWithoutSession(t *testing.T) {
	workDir := t.TempDir()
	now := time.Now().Add(-8 * 24 * time.Hour)
	cache := &imageCache{dir: filepath.Join(workDir, ".cc-connect", "attachments", "images"), active: map[string]int{}, capacity: imageCacheCapacity, now: func() time.Time { return now }, freeSpace: imageDiskFree}
	paths, release, err := cache.acquire("old", []core.ImageAttachment{{MimeType: "image/png", Data: fixtureImage(t)}})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := New(map[string]any{"work_dir": workDir, "run_as_user": "synthetic-no-spawn"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("expired cache survived agent startup without a session")
	}
}

func TestImageCacheCapacityExpiryAndActiveReaders(t *testing.T) {
	data := fixtureImage(t)
	img := []core.ImageAttachment{{MimeType: "image/png", Data: data}}
	now := time.Now()
	c := newTestImageCache(t, int64(len(data)*2), &now)
	first, releaseFirst, err := c.acquire("first", img)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	second, releaseSecond, err := c.acquire("second", img)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.acquire("third", img); err == nil {
		t.Fatal("evicted an active image")
	}
	releaseFirst()
	now = now.Add(time.Second)
	third, releaseThird, err := c.acquire("third", img)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first[0]); !os.IsNotExist(err) {
		t.Fatal("oldest unleased image was not evicted")
	}
	now = now.Add(8 * 24 * time.Hour)
	c.cleanup()
	for _, path := range append(second, third...) {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("active image expired", err)
		}
	}
	releaseSecond()
	releaseThird()
	for _, path := range append(second, third...) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("expired image retained after release")
		}
	}
}

func TestImageCacheResumeProtectsOnlyItsSessionAndPreservesOtherFiles(t *testing.T) {
	now := time.Now()
	data := fixtureImage(t)
	img := []core.ImageAttachment{{MimeType: "image/png", Data: data}}
	c := newTestImageCache(t, int64(len(data)), &now)
	paths, release, err := c.acquire("session", img)
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, resume, err := c.acquire("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.acquire("different", img); err == nil {
		t.Fatal("resume image not protected")
	}
	unknown := filepath.Join(c.dir, "business-ledger.sqlite")
	if err := os.WriteFile(unknown, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	resume()
	now = now.Add(8 * 24 * time.Hour)
	c.cleanup()
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("stale owned cache not removed")
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "unrelated" {
		t.Fatal("cleanup touched unrelated file")
	}
}

func TestImageCacheRejectsSymlinksAndLowDisk(t *testing.T) {
	now := time.Now()
	data := fixtureImage(t)
	img := []core.ImageAttachment{{MimeType: "image/png", Data: data}}
	c := newTestImageCache(t, 512<<20, &now)
	c.freeSpace = func(string) (uint64, error) { return (1 << 30) + uint64(4*len(data)) - 1, nil }
	if _, _, err := c.acquire("session", img); err == nil {
		t.Fatal("low disk accepted")
	}
	c.freeSpace = func(string) (uint64, error) { return (1 << 30) + uint64(4*len(data)), nil }
	_, release, err := c.acquire("session", img)
	if err != nil {
		t.Fatal("exact threshold rejected", err)
	}
	release()
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "cache-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink creation unavailable on this OS")
	}
	c.dir = link
	if _, _, err := c.acquire("session", img); err == nil {
		t.Fatal("symlink cache accepted")
	}
}

func TestImageCacheConcurrentReservationsStayWithinCapacity(t *testing.T) {
	now := time.Now()
	data := fixtureImage(t)
	img := []core.ImageAttachment{{MimeType: "image/png", Data: data}}
	c := newTestImageCache(t, int64(4*len(data)), &now)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := c.acquire("session", img)
			if err == nil {
				release()
			}
		}()
	}
	wg.Wait()
	root, err := c.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	entries, err := c.entries(root)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		total += entry.size
	}
	if total > c.capacity || len(c.active) != 0 {
		t.Fatalf("total=%d active=%d", total, len(c.active))
	}
}

func TestSendCompleteImagesInTextOrderAndRejectsPartialBatch(t *testing.T) {
	data := fixtureImage(t)
	stdin := &imageTestStdin{}
	cs := &claudeSession{workDir: t.TempDir(), stdin: stdin}
	cs.alive.Store(true)
	images := []core.ImageAttachment{{Data: data, PromptMarker: "[first]"}, {Data: data, PromptMarker: "[second]"}}
	if err := cs.Send("before [first] between [second] after", "fixture", images, nil); err != nil {
		t.Fatal(err)
	}
	defer cs.releaseImages()
	var input struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(stdin.Bytes(), &input); err != nil {
		t.Fatal(err)
	}
	want := []string{"text", "image", "text", "image", "text"}
	if len(input.Message.Content) != len(want) {
		t.Fatalf("unexpected parts: %d", len(input.Message.Content))
	}
	for i, part := range input.Message.Content {
		if part["type"] != want[i] {
			t.Fatalf("part %d out of order", i)
		}
		if part["type"] == "image" {
			source := part["source"].(map[string]any)
			decoded, err := base64.StdEncoding.DecodeString(source["data"].(string))
			if err != nil || !bytes.Equal(decoded, data) {
				t.Fatal("image bytes changed")
			}
		}
	}
	stdin.Reset()
	images[1].Data = []byte("broken")
	if err := cs.Send("do not send partial text", "fixture-bad", images, nil); err == nil || stdin.Len() != 0 {
		t.Fatal("partial batch reached model")
	}
}
