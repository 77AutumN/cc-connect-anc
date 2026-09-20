package files

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"regexp"
	"strings"
)

const maxExpandedBytes = 100 << 20

func mimeType(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".docx") {
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	}
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
}

// Validate existing OOXML packaging, not document creation. No macros, embedded
// objects, external relationships, active fields/formulas or archive extraction.
func validateOOXML(name string, data []byte) error {
	ext := strings.ToLower(path.Ext(name))
	if len(data) == 0 || len(data) > MaxFileBytes || (ext != ".docx" && ext != ".xlsx") {
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
		if strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels") {
			flags, err := inspectXML(f.Name, part, mainPart, mainType, rootTag)
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

func inspectXML(name string, data []byte, mainPart, mainType, rootTag string) ([3]bool, error) {
	var flags [3]bool
	d := xml.NewDecoder(bytes.NewReader(data))
	relations := strings.HasSuffix(strings.ToLower(name), ".rels")
	depth, roots := 0, 0
	formulaDepth := 0
	var formula strings.Builder
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
				if name == mainPart && t.Name.Local == rootTag && ((rootTag == "document" && t.Name.Space == "http://schemas.openxmlformats.org/wordprocessingml/2006/main") || (rootTag == "workbook" && t.Name.Space == "http://schemas.openxmlformats.org/spreadsheetml/2006/main")) {
					flags[2] = true
				}
			}
			attrs := map[string]string{}
			for _, a := range t.Attr {
				attrs[a.Name.Local] = a.Value
			}
			if name == "[Content_Types].xml" {
				value := strings.ToLower(attrs["ContentType"])
				for _, bad := range []string{"macroenabled", "vbaproject", "oleobject", "activex"} {
					if strings.Contains(value, bad) {
						return flags, ErrInvalid
					}
				}
				if t.Name.Local == "Override" && attrs["PartName"] == "/"+mainPart && attrs["ContentType"] == mainType {
					flags[0] = true
				}
			}
			if relations && t.Name.Local == "Relationship" {
				if strings.EqualFold(attrs["TargetMode"], "External") || strings.ContainsAny(attrs["Target"], ":\\") || strings.HasPrefix(attrs["Target"], "//") {
					return flags, ErrInvalid
				}
				if name == "_rels/.rels" && strings.HasSuffix(attrs["Type"], "/officeDocument") && strings.TrimPrefix(attrs["Target"], "/") == mainPart {
					flags[1] = true
				}
			}
			if t.Name.Local == "altChunk" || t.Name.Local == "object" || t.Name.Local == "oleObject" || t.Name.Local == "control" {
				return flags, ErrInvalid
			}
			if t.Name.Local == "fldSimple" && activeExpression(attrs["instr"]) {
				return flags, ErrInvalid
			}
			if t.Name.Local == "f" || t.Name.Local == "instrText" {
				formulaDepth = depth
				formula.Reset()
			}
		case xml.CharData:
			if formulaDepth > 0 {
				formula.Write(t)
			}
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return flags, ErrInvalid
			}
		case xml.EndElement:
			if depth == formulaDepth {
				if activeExpression(formula.String()) {
					return flags, ErrInvalid
				}
				formulaDepth = 0
			}
			depth--
		}
	}
	if roots != 1 || depth != 0 {
		return flags, ErrInvalid
	}
	return flags, nil
}

var activeFunction = regexp.MustCompile(`(?i)(?:^|[^A-Z0-9_])(?:DDE(?:AUTO)?|INCLUDETEXT|INCLUDEPICTURE|WEBSERVICE|HYPERLINK|RTD|CALL|EXEC)(?:[^A-Z0-9_]|$)`)
var externalBook = regexp.MustCompile(`(?i)\[(?:[^\]]*\.(?:xlsx?|xlsm|xlsb|ods|csv)|[0-9]+)\]`)

func activeExpression(value string) bool {
	return activeFunction.MatchString(value) || externalBook.MatchString(value)
}
