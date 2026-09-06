package claudecode

import (
	"github.com/chenhg5/cc-connect/core"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestImageRootRejectsAncestorSymlinkSwap(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(base, "parent")
	parked := filepath.Join(base, "parked")
	if err := os.Mkdir(parent, 0750); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(parent, parked) == nil {
				_ = os.Symlink(outside, parent)
				_ = os.Remove(parent)
				_ = os.Rename(parked, parent)
			}
		}
	}()
	for i := 0; i < 500; i++ {
		root, err := openImageCacheRoot(filepath.Join(parent, "images"), true)
		if err != nil {
			continue
		}
		f, err := root.OpenFile("fixture", os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			_ = f.Close()
		}
		_ = root.Close()
	}
	close(stop)
	wg.Wait()
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("cache operation escaped through a swapped ancestor")
	}
}

func TestImageCacheCrossUnixUserReadOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated Linux supervisor as root")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no separate test account")
	}
	gid, _ := strconv.Atoi(account.Gid)
	now := time.Now()
	c := newTestImageCache(t, 1<<20, &now)
	workspace := filepath.Dir(c.dir)
	for _, dir := range []string{filepath.Dir(workspace), workspace} {
		if err := os.Chmod(dir, 0751); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(c.dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(c.dir, 0, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.dir, os.ModeSetgid|0750); err != nil {
		t.Fatal(err)
	}
	data := fixtureImage(t)
	paths, release, err := c.acquire("fixture", []core.ImageAttachment{{MimeType: "image/png", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	probe := `import pathlib,sys
p=pathlib.Path(sys.argv[1])
assert p.read_bytes().startswith(b'\x89PNG')
try:
 p.open('ab')
except PermissionError:
 pass
else:
 raise RuntimeError('model can write the image cache')
`
	if out, err := exec.Command("runuser", "-u", "nobody", "--", "python3", "-c", probe, paths[0]).CombinedOutput(); err != nil {
		t.Fatalf("separate Unix user cannot safely read image: %v %s", err, out)
	}
}
