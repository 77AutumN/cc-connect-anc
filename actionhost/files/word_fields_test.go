package files

import (
	"bytes"
	"encoding/xml"
	"testing"
)

func escaped(value string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

func TestWordPaginationFieldsAreExplicitAndBalanced(t *testing.T) {
	complex := func(code string) string {
		return `<w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText>` + escaped(code) + `</w:instrText></w:r><w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r><w:t>1</w:t></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r>`
	}
	for _, code := range []string{"PAGE", `NUMPAGES \* Arabic`, `TOC \o "1-3" \h \z \u`, `PAGEREF _Toc123 \h`} {
		for _, simple := range []bool{false, true} {
			body := complex(code)
			if simple {
				body = `<w:fldSimple w:instr="` + escaped(code) + `"><w:r><w:t>1</w:t></w:r></w:fldSimple>`
			}
			p := documentParts("docx")
			p["word/header1.xml"] = `<w:hdr xmlns:w="` + wordNamespace + `"><w:p>` + body + `</w:p></w:hdr>`
			data := packageBytes(t, p)
			if validateOOXML("old.docx", data) == nil {
				t.Fatal("default field behavior changed")
			}
			if err := validateOffice("page.docx", data, true); err != nil {
				t.Fatal(code, simple, err)
			}
		}
	}
	for _, code := range []string{`INCLUDETEXT "https://example.invalid"`, `DDEAUTO cmd`, `HYPERLINK "https://example.invalid"`, `PAGE \a "file:///private"`, `PAGEREF "https://example.invalid"`, `TOC \f "external"`, "PAGE INCLUDETEXT", ""} {
		if staticWordField(code) {
			t.Fatal("active/unsupported field accepted", code)
		}
	}
	for _, body := range []string{
		`<w:instrText>PAGE</w:instrText>`,
		`<w:fldChar w:fldCharType="end"/>`,
		`<w:fldChar w:fldCharType="begin"/><w:instrText>PAGE</w:instrText>`,
		`<w:fldChar w:fldCharType="begin"/><w:instrText>PAGE</w:instrText><w:fldChar w:fldCharType="begin"/>`,
		`<w:fldChar w:fldCharType="begin"/><w:instrText>INCLUDE</w:instrText><w:instrText>TEXT</w:instrText><w:fldChar w:fldCharType="end"/>`,
		`<w:fldSimple xmlns:x="urn:fictional" w:instr="INCLUDETEXT" x:instr="PAGE"/>`,
	} {
		p := documentParts("docx")
		p["word/footer1.xml"] = `<w:ftr xmlns:w="` + wordNamespace + `">` + body + `</w:ftr>`
		if validateOffice("bad.docx", packageBytes(t, p), true) == nil {
			t.Fatal("malformed field accepted", body)
		}
	}
	// A TOC result contains nested local page references, not nested instructions.
	p := documentParts("docx")
	p["word/footer1.xml"] = `<w:ftr xmlns:w="` + wordNamespace + `"><w:fldChar w:fldCharType="begin"/><w:instrText>TOC \o "1-3" \h</w:instrText><w:fldChar w:fldCharType="separate"/>` + complex(`PAGEREF _Toc123 \h`) + `<w:fldChar w:fldCharType="end"/></w:ftr>`
	if err := validateOffice("toc.docx", packageBytes(t, p), true); err != nil {
		t.Fatal(err)
	}
}
