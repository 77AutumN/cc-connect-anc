package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"github.com/chenhg5/cc-connect/core"
	"image"
	"image/png"
	"testing"
)

// Locally generated lossless 32x32 green square, no customer or third-party image.
const stillWebPFixture = "UklGRhwAAABXRUJQVlA4TA8AAAAvH8AHAAdQwIj+ByKi/wEA"

func TestImagesStaticAndAnimatedWebPFirstFrame(t *testing.T) {
	still, err := base64.StdEncoding.DecodeString(stillWebPFixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(still))
	if err != nil {
		t.Fatal(err)
	}
	static, err := prepareImages([]core.ImageAttachment{{Data: still}})
	if err != nil || !bytes.Equal(static[0].Data, still) {
		t.Fatal("static WebP changed")
	}
	chunk := func(tag string, data []byte) []byte {
		b := make([]byte, 8+len(data)+(len(data)&1))
		copy(b, tag)
		binary.LittleEndian.PutUint32(b[4:8], uint32(len(data)))
		copy(b[8:], data)
		return b
	}
	u24 := func(b []byte, n int) { b[0] = byte(n); b[1] = byte(n >> 8); b[2] = byte(n >> 16) }
	canvas := make([]byte, 10)
	canvas[0] = 2
	u24(canvas[4:7], cfg.Width-1)
	u24(canvas[7:10], cfg.Height-1)
	frame := make([]byte, 16)
	u24(frame[6:9], cfg.Width-1)
	u24(frame[9:12], cfg.Height-1)
	frame[12] = 1
	frame[15] = 2
	frame = append(frame, still[12:]...)
	body := append([]byte("WEBP"), chunk("VP8X", canvas)...)
	body = append(body, chunk("ANIM", make([]byte, 6))...)
	body = append(body, chunk("ANMF", frame)...)
	// Later frames must not be decoded or shown.
	body = append(body, chunk("ANMF", []byte("later frame deliberately invalid"))...)
	animated := make([]byte, 8)
	copy(animated, "RIFF")
	binary.LittleEndian.PutUint32(animated[4:], uint32(len(body)))
	animated = append(animated, body...)
	prepared, err := prepareImages([]core.ImageAttachment{{Data: animated}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := png.Decode(bytes.NewReader(prepared[0].Data))
	if err != nil || first.Bounds().Dx() != cfg.Width || first.Bounds().Dy() != cfg.Height {
		t.Fatal("first frame resolution changed")
	}
	want, _, _ := image.Decode(bytes.NewReader(still))
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			r, g, b, a := want.At(x, y).RGBA()
			r2, g2, b2, a2 := first.At(x, y).RGBA()
			if r != r2 || g != g2 || b != b2 || a != a2 {
				t.Fatal("first frame pixels changed")
			}
		}
	}
	for _, data := range [][]byte{animated[:29], animated[:len(animated)-2]} {
		if _, err := prepareImages([]core.ImageAttachment{{Data: data}}); err == nil {
			t.Fatal("truncated WebP accepted")
		}
	}
	bomb := append([]byte(nil), animated...)
	u24(bomb[24:27], 8000)
	if _, err := prepareImages([]core.ImageAttachment{{Data: bomb}}); err == nil {
		t.Fatal("oversize canvas accepted")
	}
}
