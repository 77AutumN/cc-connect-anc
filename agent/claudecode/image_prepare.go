package claudecode

import (
	"bytes"
	"github.com/chenhg5/cc-connect/core"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
)

// prepareImages validates the entire batch before the first model input byte.
// Animated GIFs become the first frame at original resolution, not thumbnails.
func prepareImages(images []core.ImageAttachment) ([]core.ImageAttachment, error) {
	if err := core.CheckImageBatch(images); err != nil {
		return nil, err
	}
	prepared := append([]core.ImageAttachment(nil), images...)
	for i, img := range images {
		_, mime, err := core.ReadImage(bytes.NewReader(img.Data))
		if err != nil {
			return nil, err
		}
		if mime == "image/webp" {
			first, animated, err := firstWebPFrame(img.Data)
			if err != nil {
				return nil, err
			}
			if animated {
				prepared[i].Data, prepared[i].MimeType = first, "image/png"
				continue
			}
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(img.Data))
		if err != nil {
			return nil, &core.ImageInputError{Key: core.MsgImageInvalid}
		}
		// Match the native vision edge bound and reject decompression bombs
		// before allocating a pixel buffer. Never resize to hide this failure.
		if config.Width <= 0 || config.Height <= 0 || config.Width > 8000 || config.Height > 8000 {
			return nil, &core.ImageInputError{Key: core.MsgImageDimensions}
		}
		decoded, _, err := image.Decode(bytes.NewReader(img.Data))
		if err != nil {
			return nil, &core.ImageInputError{Key: core.MsgImageInvalid}
		}
		prepared[i].MimeType = mime
		if mime == "image/gif" {
			// Decode consumes only the first frame. Retain the logical canvas,
			// including an offset frame, without resizing or allocating later frames.
			canvas := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
			draw.Draw(canvas, decoded.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
			first, err := encodeFirstImageFrame(canvas)
			if err != nil {
				return nil, err
			}
			prepared[i].Data, prepared[i].MimeType = first, "image/png"
		}
	}
	if err := core.CheckImageBatch(prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

type imageFrameBuffer struct{ bytes.Buffer }

func (w *imageFrameBuffer) Write(p []byte) (int, error) {
	if w.Len()+len(p) > core.MaxImageBytes {
		return 0, &core.ImageInputError{Key: core.MsgImageLimit}
	}
	return w.Buffer.Write(p)
}
func encodeFirstImageFrame(frame image.Image) ([]byte, error) {
	var output imageFrameBuffer
	if err := png.Encode(&output, frame); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
