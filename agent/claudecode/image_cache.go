package claudecode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const imageCacheCapacity int64 = 512 << 20
const imageCacheRetention = 7 * 24 * time.Hour

// Names carry only expiry and an opaque session digest; no user filenames or
// customer metadata. Unknown entries, directories and symlinks are never removed.
var cacheImageName = regexp.MustCompile(`^cci-([0-9]{20})-([a-f0-9]{16})-[a-f0-9]{16}\.(png|jpg|gif|webp)(\.tmp)?$`)

type imageCache struct {
	mu        sync.Mutex
	dir       string
	active    map[string]int
	capacity  int64
	now       func() time.Time
	freeSpace func(string) (uint64, error)
}

var imageCaches = struct {
	sync.Mutex
	byDir map[string]*imageCache
	once  sync.Once
}{byDir: make(map[string]*imageCache)}

func sessionImageCache(workDir string) *imageCache {
	cache, _ := configuredImageCache(workDir, 0)
	return cache
}

func configuredImageCache(workDir string, capacity int64) (*imageCache, error) {
	dir := filepath.Join(workDir, ".cc-connect", "attachments", "images")
	imageCaches.Lock()
	defer imageCaches.Unlock()
	cache := imageCaches.byDir[dir]
	if cache != nil && capacity != 0 && cache.capacity != capacity {
		return nil, fmt.Errorf("image cache capacity cannot change while registered")
	}
	if cache == nil {
		if capacity == 0 {
			capacity = imageCacheCapacity
		}
		cache = &imageCache{dir: dir, active: make(map[string]int), capacity: capacity, now: time.Now, freeSpace: imageDiskFree}
		imageCaches.byDir[dir] = cache
	}
	// One lightweight sweeper for all gateway-owned image caches, not a service.
	imageCaches.once.Do(func() {
		go func() {
			for range time.NewTicker(time.Hour).C {
				imageCaches.Lock()
				caches := make([]*imageCache, 0, len(imageCaches.byDir))
				for _, c := range imageCaches.byDir {
					caches = append(caches, c)
				}
				imageCaches.Unlock()
				for _, c := range caches {
					c.cleanup()
				}
			}
		}()
	})
	return cache, nil
}

func (c *imageCache) open(create bool) (*os.Root, error) { return openImageCacheRoot(c.dir, create) }

// These handles are directories; image data writes check Sync and Close separately.
func closeImageCacheDirectory(handle io.Closer) {
	if err := handle.Close(); err != nil {
		slog.Warn("image cache directory close failed")
	}
}

type cachedImage struct {
	name, session string
	created       time.Time
	size          int64
}

func (c *imageCache) entries(root *os.Root) ([]cachedImage, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer closeImageCacheDirectory(f)
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var images []cachedImage
	for _, entry := range entries {
		match := cacheImageName.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		nanos, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			continue
		}
		images = append(images, cachedImage{entry.Name(), match[2], time.Unix(0, nanos), info.Size()})
	}
	sort.Slice(images, func(i, j int) bool { return images[i].name < images[j].name })
	return images, nil
}

// reserve runs under mu, including all temporary writes. Concurrent batches
// cannot overbook capacity; active readers remain protected during eviction.
func (c *imageCache) reserve(root *os.Root, incoming int64) error {
	entries, err := c.entries(root)
	if err != nil {
		return err
	}
	total := incoming
	for _, entry := range entries {
		total += entry.size
	}
	for _, entry := range entries {
		if c.active[entry.name] > 0 {
			continue
		}
		if total <= c.capacity && c.now().Sub(entry.created) < imageCacheRetention && !strings.HasSuffix(entry.name, ".tmp") {
			continue
		}
		// The rooted relative path was checked against our exact ownership format.
		info, err := root.Lstat(entry.name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("image cache entry changed")
		}
		if err := root.Remove(entry.name); err != nil {
			return err
		}
		total -= entry.size
	}
	if total > c.capacity {
		return fmt.Errorf("image cache capacity in use")
	}
	return nil
}

func (c *imageCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	root, err := c.open(false)
	if os.IsNotExist(err) {
		return
	}
	if err == nil {
		defer closeImageCacheDirectory(root)
		err = c.reserve(root, 0)
	}
	if err != nil {
		slog.Warn("image cache cleanup unavailable")
	}
}

func imageSessionDigest(key string) string {
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:8])
}

// Called with c.mu held. Register the temporary name before writing so the
// caller's batch rollback owns every partial and completed file.
func (c *imageCache) writeImage(root *os.Root, digest string, img core.ImageAttachment, created *[]string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := fmt.Sprintf("cci-%020d-%s-%x%s", c.now().UnixNano(), digest, random, extFromMime(img.MimeType))
	temp := name + ".tmp"
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	*created = append(*created, temp)
	_, err = f.Write(img.Data)
	if err == nil {
		err = f.Chmod(0640)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := root.Rename(temp, name); err != nil {
		return "", err
	}
	(*created)[len(*created)-1] = name
	return name, nil
}

func (c *imageCache) acquire(session string, images []core.ImageAttachment) ([]string, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func() ([]string, func(), error) {
		return nil, nil, &core.ImageInputError{Key: core.MsgImageCacheFailed}
	}
	root, err := c.open(len(images) > 0)
	if os.IsNotExist(err) && len(images) == 0 {
		return nil, func() {}, nil
	}
	if err != nil {
		return fail()
	}
	defer closeImageCacheDirectory(root)
	// Expire first, so starting a new turn cannot indefinitely retain stale data.
	if err := c.reserve(root, 0); err != nil {
		return fail()
	}
	entries, err := c.entries(root)
	if err != nil {
		return fail()
	}
	digest := imageSessionDigest(session)
	var held []string
	for _, entry := range entries {
		if entry.session == digest && !strings.HasSuffix(entry.name, ".tmp") {
			held = append(held, entry.name)
			c.active[entry.name]++
		}
	}
	unhold := func() {
		for _, name := range held {
			c.active[name]--
			if c.active[name] == 0 {
				delete(c.active, name)
			}
		}
	}
	var size int64
	for _, img := range images {
		size += int64(len(img.Data))
	}
	if err := c.reserve(root, size); err != nil {
		unhold()
		return fail()
	}
	if len(images) > 0 {
		free, err := c.freeSpace(c.dir)
		if err != nil {
			unhold()
			return fail()
		}
		if free < uint64((1<<30)+4*size) {
			unhold()
			return nil, nil, &core.ImageInputError{Key: core.MsgImageDiskLow}
		}
	}
	var paths, created []string
	rollback := func() {
		for _, name := range created {
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				slog.Warn("image cache incomplete batch cleanup failed")
			}
		}
		unhold()
	}
	for _, img := range images {
		name, err := c.writeImage(root, digest, img, &created)
		if err != nil {
			rollback()
			return fail()
		}
		held = append(held, name)
		c.active[name]++
		paths = append(paths, filepath.Join(c.dir, name))
	}
	var once sync.Once
	release := func() { once.Do(func() { c.mu.Lock(); unhold(); c.mu.Unlock(); c.cleanup() }) }
	return paths, release, nil
}
