package claudecode

import (
	"bytes"
	"github.com/chenhg5/cc-connect/core"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"testing"
)

func TestReadImageRejectsUnknownAndShortRIFF(t *testing.T) {
	for n := 0; n < 12; n++ {
		data := append([]byte("RIFF"), bytes.Repeat([]byte{0}, n)...)
		if _, _, err := core.ReadImage(bytes.NewReader(data)); err == nil {
			t.Fatal("invalid RIFF accepted")
		}
	}
	if _, _, err := core.ReadImage(bytes.NewReader([]byte("not an image"))); err == nil {
		t.Fatal("unknown input defaulted to PNG")
	}
}

type endlessImageReader struct{ read int }

func (r *endlessImageReader) Read(p []byte) (int, error) {
	clear(p)
	r.read += len(p)
	return len(p), nil
}
func TestImageDownloadReadsAtMostLimitPlusOne(t *testing.T) {
	r := &endlessImageReader{}
	if _, _, err := core.ReadImage(r); err == nil {
		t.Fatal("oversize accepted")
	}
	if r.read != core.MaxImageBytes+1 {
		t.Fatalf("read %d bytes", r.read)
	}
}

func TestPrepareImagesGIFUsesFirstFrame(t *testing.T) {
	palette := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(first.Rect, palette)
	for i := range second.Pix {
		second.Pix[i] = 1
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareImages([]core.ImageAttachment{{Data: buf.Bytes()}})
	if err != nil {
		t.Fatal(err)
	}
	if prepared[0].MimeType != "image/png" {
		t.Fatal("animation not frozen")
	}
	decoded, err := png.Decode(bytes.NewReader(prepared[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, b, _ := decoded.At(0, 0).RGBA()
	if r == 0 || b != 0 {
		t.Fatal("not the first frame")
	}
}

func TestImageBatchLimitsAndAtomicValidation(t *testing.T) {
	if core.CheckImageBatch(make([]core.ImageAttachment, 5)) == nil {
		t.Fatal("five images accepted")
	}
	if core.CheckImageBatch([]core.ImageAttachment{{Data: make([]byte, core.MaxImageBytes+1)}}) == nil {
		t.Fatal("per-image limit missing")
	}
	if core.CheckImageBatch([]core.ImageAttachment{{Data: make([]byte, core.MaxImageBytes)}, {Data: make([]byte, core.MaxImageBytes)}, {Data: []byte{1}}}) == nil {
		t.Fatal("aggregate limit missing")
	}
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	if images, err := prepareImages([]core.ImageAttachment{{Data: buf.Bytes()}, {Data: []byte("invalid")}}); err == nil || images != nil {
		t.Fatal("partial validation returned usable images")
	}
	if _, _, err := core.ReadImage(io.MultiReader(bytes.NewReader(buf.Bytes()), errorImageReader{})); err == nil {
		t.Fatal("partial download accepted")
	}
}

type errorImageReader struct{}

func (errorImageReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
