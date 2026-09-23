package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"regexp"
	"strings"
)

const maxExpandedBytes = 100 << 20

// ValidateOfficeDocument also guards the local ordinary-user renderer before
// LibreOffice opens a generated document. It performs no storage or delivery.
func ValidateOfficeDocument(name string, data []byte) error {
	return validateOffice(name, data, true)
}

func mimeType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	}
	return "application/octet-stream"
}

// Validate existing OOXML packaging, not document creation. No macros, embedded
// objects, external relationships, active fields/formulas or archive extraction.
func validateOOXML(name string, data []byte) error {
	return validateOffice(name, data, false)
}

func validateOffice(name string, data []byte, extended bool) error {
	ext := strings.ToLower(path.Ext(name))
	if len(data) == 0 || len(data) > MaxFileBytes || (ext != ".docx" && ext != ".xlsx" && (!extended || ext != ".pptx")) {
		return ErrInvalid
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(z.File) == 0 || len(z.File) > 10000 {
		return ErrInvalid
	}
	mainPart, mainType, rootTag := "word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml", "document"
	if ext == ".xlsx" {
		mainPart, mainType, rootTag = "xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml", "workbook"
	}
	if ext == ".pptx" {
		mainPart, mainType, rootTag = "ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml", "presentation"
	}
	seen := map[string]bool{}
	var expanded uint64
	contentType, rootRelation, mainDocument := false, false, false
	for _, f := range z.File {
		lower := strings.ToLower(f.Name)
		if f.FileInfo().IsDir() {
			continue
		}
		if !relativeFile(f.Name) || seen[lower] || !f.Mode().IsRegular() || f.Flags&1 != 0 || f.UncompressedSize64 > maxExpandedBytes || expanded > maxExpandedBytes-f.UncompressedSize64 {
			return ErrInvalid
		}
		seen[lower], expanded = true, expanded+f.UncompressedSize64
		for _, forbidden := range []string{"vbaproject", "vbasignature", "activex/", "embeddings/", "externallinks/", "customui/", "webextensions/"} {
			if strings.Contains(lower, forbidden) {
				return ErrInvalid
			}
		}
		r, err := f.Open()
		if err != nil {
			return ErrInvalid
		}
		part, readErr := io.ReadAll(io.LimitReader(r, int64(f.UncompressedSize64)+1))
		closeErr := r.Close()
		if readErr != nil || closeErr != nil || uint64(len(part)) != f.UncompressedSize64 {
			return ErrInvalid
		}
		// OPC targets can live anywhere in a package. Restrict every binary
		// part, rather than trusting the conventional ppt/media directory.
		// python-pptx's stock template includes opaque, non-executable printer
		// metadata. Keep it bounded; package/OLE relationships remain forbidden.
		printerSettings := strings.HasPrefix(lower, "ppt/printersettings/") && strings.HasSuffix(lower, ".bin") && len(part) <= 64<<10
		if ext == ".pptx" && !strings.HasSuffix(lower, ".xml") && !strings.HasSuffix(lower, ".rels") && !printerSettings && validateImage(f.Name, part) != nil {
			return ErrInvalid
		}
		if strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels") {
			flags, err := inspectXML(f.Name, part, mainPart, mainType, rootTag, extended && ext == ".docx")
			if err != nil {
				return err
			}
			contentType, rootRelation, mainDocument = contentType || flags[0], rootRelation || flags[1], mainDocument || flags[2]
		}
	}
	if !contentType || !rootRelation || !mainDocument {
		return ErrInvalid
	}
	return nil
}

func inspectXML(name string, data []byte, mainPart, mainType, rootTag string, allowFields bool) ([3]bool, error) {
	var flags [3]bool
	// A UTF-8 BOM is an encoding signature only at the start of an XML part.
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	var defaultType, overrideType string
	defaultSeen, overrideSeen := false, false
	relations := strings.HasSuffix(strings.ToLower(name), ".rels")
	depth, roots := 0, 0
	formulaDepth := 0
	var formula strings.Builder
	var fields wordFields
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return flags, ErrInvalid
		}
		switch t := token.(type) {
		case xml.Directive:
			return flags, ErrInvalid
		case xml.StartElement:
			depth++
			if depth > 256 {
				return flags, ErrInvalid
			}
			if depth == 1 {
				roots++
				if (name == "[Content_Types].xml" && (t.Name.Local != "Types" || t.Name.Space != "http://schemas.openxmlformats.org/package/2006/content-types")) || (relations && (t.Name.Local != "Relationships" || t.Name.Space != "http://schemas.openxmlformats.org/package/2006/relationships")) {
					return flags, ErrInvalid
				}
				if name == mainPart && t.Name.Local == rootTag && ((rootTag == "document" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main") || (rootTag == "workbook" && t.Name.Space == "http://schemas.openxmlformats.org/spreadsheetml/2006/main") || (rootTag == "presentation" && t.Name.Space == "http://schemas.openxmlformats.org/presentationml/2006/main")) {
					flags[2] = true
				}
			}
			attrs := map[xml.Name]string{}
			for _, a := range t.Attr {
				if _, duplicate := attrs[a.Name]; duplicate {
					return flags, ErrInvalid
				}
				attrs[a.Name] = a.Value
			}
			// Package metadata attributes are unqualified. A foreign attribute
			// with the same local name cannot replace its authoritative value.
			attr := func(local string) string { return attrs[xml.Name{Local: local}] }
			if name == "[Content_Types].xml" {
				value := strings.ToLower(attr("ContentType"))
				for _, bad := range []string{"macroenabled", "vbaproject", "oleobject", "activex"} {
					if strings.Contains(value, bad) {
						return flags, ErrInvalid
					}
				}
				if depth == 2 && t.Name.Space == "http://schemas.openxmlformats.org/package/2006/content-types" {
					switch {
					case t.Name.Local == "Default" && strings.EqualFold(attr("Extension"), strings.TrimPrefix(path.Ext(mainPart), ".")):
						if defaultSeen {
							return flags, ErrInvalid
						}
						defaultType, defaultSeen = attr("ContentType"), true
					// mainPart is ASCII: equal byte length excludes Unicode fold
					// aliases such as Kelvin sign, which OPC does not equate to K.
					case t.Name.Local == "Override" && len(attr("PartName")) == len(mainPart)+1 && strings.EqualFold(attr("PartName"), "/"+mainPart):
						if overrideSeen {
							return flags, ErrInvalid
						}
						overrideType, overrideSeen = attr("ContentType"), true
					}
				}
			}
			if relations && t.Name.Local == "Relationship" {
				if t.Name.Space != "http://schemas.openxmlformats.org/package/2006/relationships" || strings.EqualFold(attr("TargetMode"), "External") || strings.ContainsAny(attr("Target"), ":\\") || strings.HasPrefix(attr("Target"), "//") {
					return flags, ErrInvalid
				}
				if rootTag == "presentation" {
					switch path.Base(attr("Type")) {
					case "oleObject", "package", "control", "audio", "video", "media":
						return flags, ErrInvalid
					case "image":
						ext := strings.ToLower(path.Ext(attr("Target")))
						if ext != ".png" && ext != ".jpeg" && ext != ".jpg" {
							return flags, ErrInvalid
						}
					}
				}
				if name == "_rels/.rels" && strings.HasSuffix(attr("Type"), "/officeDocument") && strings.TrimPrefix(attr("Target"), "/") == mainPart {
					flags[1] = true
				}
			}
			if t.Name.Local == "altChunk" || t.Name.Local == "object" || t.Name.Local == "oleObject" || t.Name.Local == "oleObj" || t.Name.Local == "control" || t.Name.Local == "audio" || t.Name.Local == "video" || attr("action") != "" {
				return flags, ErrInvalid
			}
			if fields.start(t, allowFields) != nil {
				return flags, ErrInvalid
			}
			if t.Name.Local == "f" {
				formulaDepth = depth
				formula.Reset()
			}
		case xml.CharData:
			if fields.text(t) != nil {
				return flags, ErrInvalid
			}
			if formulaDepth > 0 {
				formula.Write(t)
			}
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return flags, ErrInvalid
			}
		case xml.EndElement:
			if t.Name.Local == "instrText" {
				fields.instruction = false
			}
			if depth == formulaDepth {
				if activeExpression(formula.String()) {
					return flags, ErrInvalid
				}
				formulaDepth = 0
			}
			depth--
		}
	}
	if roots != 1 || depth != 0 || len(fields.stack) != 0 || fields.instruction {
		return flags, ErrInvalid
	}
	// OPC resolves a part-specific override before its extension default,
	// regardless of declaration order. A bad override cannot fall back.
	flags[0] = defaultSeen && defaultType == mainType
	if overrideSeen {
		flags[0] = overrideType == mainType
	}
	return flags, nil
}

// Decode the complete image after bounding its allocation, not just its magic.
func validateImage(name string, data []byte) error {
	if len(data) == 0 || len(data) > MaxFileBytes {
		return ErrInvalid
	}
	want := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if want == "jpg" {
		want = "jpeg"
	}
	if want != "png" && want != "jpeg" {
		return ErrInvalid
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != want || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 25_000_000 {
		return ErrInvalid
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return ErrInvalid
	}
	return nil
}

var activeFunction = regexp.MustCompile(`(?i)(?:^|[^A-Z0-9_])(?:DDE(?:AUTO)?|INCLUDETEXT|INCLUDEPICTURE|WEBSERVICE|HYPERLINK|RTD|CALL|EXEC)(?:[^A-Z0-9_]|$)`)
var externalBook = regexp.MustCompile(`(?i)\[(?:[^\]]*\.(?:xlsx?|xlsm|xlsb|ods|csv)|[0-9]+)\]`)

func activeExpression(value string) bool {
	return activeFunction.MatchString(value) || externalBook.MatchString(value)
}
