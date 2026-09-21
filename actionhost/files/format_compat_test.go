package files

import (
	"os"
	"strings"
	"testing"
)

// This unchanged, fictional artifact-tool workbook exposed both compatibility
// failures during the private-chat intake test. It contains no company data.
func TestOOXMLArtifactToolWorkbook(t *testing.T) {
	data, err := os.ReadFile("testdata/fictional-demand.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOOXML("fictional-demand.xlsx", data); err != nil {
		t.Fatal("ordinary workbook rejected", err)
	}
}

func TestOOXMLBOMAndDefaultContentType(t *testing.T) {
	for _, ext := range []string{"docx", "xlsx"} {
		t.Run(ext, func(t *testing.T) {
			parts := documentParts(ext)
			override := strings.TrimSuffix(strings.SplitN(parts["[Content_Types].xml"], ">", 2)[1], "</Types>")
			defaultType := strings.Replace(override, "Override PartName=\"/word/document.xml\"", "Default Extension=\"xml\"", 1)
			defaultType = strings.Replace(defaultType, "Override PartName=\"/xl/workbook.xml\"", "Default Extension=\"xml\"", 1)
			wrongOverride := strings.Replace(override, "ContentType=\"", "ContentType=\"wrong/", 1)
			wrongDefault := strings.Replace(defaultType, "ContentType=\"", "ContentType=\"wrong/", 1)
			upperOverride := strings.NewReplacer("/word/document.xml", "/WORD/DOCUMENT.XML", "/xl/workbook.xml", "/XL/WORKBOOK.XML").Replace(override)
			for _, tc := range []struct {
				name, types, prefix, suffix string
				valid                       bool
			}{
				{"bom", override, "\ufeff", "", true},
				{"default", defaultType, "", "", true},
				{"both", defaultType, "\ufeff", "", true},
				{"override_before_default", override + wrongDefault, "", "", true},
				{"override_after_default", wrongDefault + override, "", "", true},
				{"wrong_override_before", wrongOverride + defaultType, "", "", false},
				{"wrong_override_after", defaultType + wrongOverride, "", "", false},
				{"mixed_case_override", upperOverride + wrongDefault, "", "", true},
				{"mixed_case_wrong_override", defaultType + strings.Replace(upperOverride, "ContentType=\"", "ContentType=\"wrong/", 1), "", "", false},
				{"mixed_case_duplicate_override", override + upperOverride, "", "", false},
				{"duplicate_default", defaultType + defaultType, "", "", false},
				{"duplicate_override", override + override, "", "", false},
				{"foreign_default", strings.Replace(defaultType, "Default ", "Default xmlns=\"urn:foreign\" ", 1), "", "", false},
				{"nested_default", "<wrapper>" + defaultType + "</wrapper>", "", "", false},
				{"double_bom", override, "\ufeff\ufeff", "", false},
				{"misplaced_bom", override, " \ufeff", "", false},
				{"trailing_bom", override, "", "\ufeff", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					p := documentParts(ext)
					p["[Content_Types].xml"] = `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` + tc.types + `</Types>`
					for name, body := range p {
						p[name] = tc.prefix + body + tc.suffix
					}
					if err := validateOOXML("sample."+ext, packageBytes(t, p)); (err == nil) != tc.valid {
						t.Fatalf("valid=%v, error=%v", tc.valid, err)
					}
				})
			}
		})
	}
}
