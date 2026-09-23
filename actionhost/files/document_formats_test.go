package files

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func presentationParts() map[string]string {
	return map[string]string{
		"[Content_Types].xml":  `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`,
		"_rels/.rels":          `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`,
		"ppt/presentation.xml": `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:sldIdLst/></p:presentation>`,
	}
}

func fictionalImage(t *testing.T, ext string) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	if ext == "png" {
		err = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 32, 24)))
	} else {
		err = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDocumentFormatsOptInAndMalformedContent(t *testing.T) {
	old, enabled := &Host{}, &Host{documentValidator: "fixed-host-validator"}
	ctx := context.Background()
	for _, ext := range []string{"png", "jpeg", "jpg", "pptx"} {
		data := fictionalImage(t, ext)
		if ext == "pptx" {
			data = packageBytes(t, presentationParts())
		}
		name := "fictional." + ext
		if old.validateFile(ctx, name, data) == nil {
			t.Fatal("changed default", ext)
		}
		if err := enabled.validateFile(ctx, name, data); err != nil {
			t.Fatal(ext, err)
		}
		if enabled.validateFile(ctx, name, data[:len(data)/2]) == nil {
			t.Fatal("truncated content accepted", ext)
		}
	}
	if enabled.validateFile(ctx, "disguised.png", fictionalImage(t, "jpeg")) == nil {
		t.Fatal("extension mismatch accepted")
	}
	for _, part := range []string{"ppt/embeddings/chart.xlsx", "ppt/vbaProject.bin", "ppt/media/video.mp4"} {
		p := presentationParts()
		p[part] = "unsupported"
		if validateOffice("bad.pptx", packageBytes(t, p), true) == nil {
			t.Fatal(part)
		}
	}
	p := presentationParts()
	p["ppt/slides/slide1.xml"] = `<sld><hlinkClick action="ppaction://program"/></sld>`
	if validateOffice("active.pptx", packageBytes(t, p), true) == nil {
		t.Fatal("active slide action accepted")
	}
}

func TestNewFormatsUseExistingSnapshotsReceiptsAndVersions(t *testing.T) {
	calls := 0
	f := newFixture(t, func(_ context.Context, route json.RawMessage, file core.FileAttachment, _ string) (string, error) {
		calls++
		if string(route) != `{"audience":"original-thread"}` || file.MimeType != mimeType(file.FileName) {
			t.Fatal("route or MIME changed")
		}
		return fmt.Sprintf("receipt-%d", calls), nil
	})
	f.host.documentValidator = "fixed-host-validator"
	for i, ext := range []string{"png", "jpeg", "pptx"} {
		data := fictionalImage(t, ext)
		if ext == "pptx" {
			data = packageBytes(t, presentationParts())
		}
		name := "fictional." + ext
		b := f.binding
		b.Principal.MessageID = name
		b.Inputs = []core.FileAttachment{{FileName: name, Data: data}}
		w, err := f.host.Bind(context.Background(), b)
		if err != nil || len(w.Inputs) != i+1 {
			t.Fatal(w, err)
		}
		if err = os.WriteFile(filepath.Join(w.OutputDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		result, err := deliver(t, f, name, data, i)
		if err != nil || result["status"] != "accepted" {
			t.Fatal(result, err)
		}
		if _, err = deliver(t, f, name, data, i); err != nil || calls != i+1 {
			t.Fatal("duplicate resent", err)
		}
		if _, err = f.host.FindByMessage(context.Background(), b.Principal, fmt.Sprintf("receipt-%d", calls)); err != nil {
			t.Fatal("reply not bound", err)
		}
		original, err := os.ReadFile(w.Inputs[i].Path)
		if err != nil || !bytes.Equal(original, data) {
			t.Fatal("original lost", err)
		}
	}
}
