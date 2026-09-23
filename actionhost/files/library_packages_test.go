package files

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Pair the actual CRM package pins with the production gateway format boundary.
// No dependency is installed here; the caller supplies an existing interpreter.
func TestPairedOfficeLibraries(t *testing.T) {
	python, crm := os.Getenv("CC_FILE_DOCUMENT_PYTHON"), os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if python == "" || crm == "" {
		t.Skip("requires explicit document interpreter and exact CRM source")
	}
	output := t.TempDir()
	code := `import importlib.metadata as m,sys
from pathlib import Path
from docx import Document
from openpyxl import Workbook,load_workbook
from pptx import Presentation
from pptx.util import Inches
from PIL import Image
from pypdf import PdfWriter
for line in Path(sys.argv[1]).read_text().splitlines():
    if line and not line.startswith('#'):
        name,version=line.split('==')
        assert m.version(name)==version, name
root=Path(sys.argv[2])
deck=Presentation()
slide=deck.slides.add_slide(deck.slide_layouts[6])
slide.shapes.add_textbox(Inches(1),Inches(1),Inches(4),Inches(1)).text='Fictional proposal'
Image.new('RGB',(32,24),'#ab7858').save(root/'poster.png')
Image.new('RGB',(32,24),'#ab7858').save(root/'material.jpg')
slide.shapes.add_picture(str(root/'poster.png'),Inches(1),Inches(2))
deck.save(root/'proposal.pptx')
pdf=PdfWriter()
pdf.add_blank_page(width=612,height=792)
pdf.write(root/'material.pdf')
for version in (1,2):
    doc=Document()
    doc.add_heading('Fictional analysis',0)
    doc.add_paragraph('Source: fictional-policy v1; revision '+str(version))
    doc.save(root / ('analysis-v%d.docx'%version))
    book=Workbook()
    book.active.append(['Units','Price','Total'])
    book.active.append([version,20,'=A2*B2'])
    book.save(root / ('analysis-v%d.xlsx'%version))
    assert Document(root / ('analysis-v%d.docx'%version)).paragraphs[1].text.endswith(str(version))
    assert load_workbook(root / ('analysis-v%d.xlsx'%version)).active['A2'].value==version
`
	cmd := exec.Command(python, "-B", "-c", code, filepath.Join(crm, "ops", "runtime", "team-files-requirements.txt"), output)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pinned document libraries: %v: %s", err, result)
	}
	host := &Host{documentValidator: "enabled-without-PDF-on-Windows"}
	if runtime.GOOS != "windows" {
		data, err := os.ReadFile(filepath.Join(crm, "ops", "runtime", "team_file_pdf.py"))
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(data), "\n", 2)
		helper := filepath.Join(output, "validator")
		if err = os.Chmod(output, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(helper, []byte("#!"+python+" -I\n"+parts[1]), 0700); err != nil {
			t.Fatal(err)
		}
		if err = host.SetDocumentFormats(helper); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"analysis-v1.docx", "analysis-v1.xlsx", "analysis-v2.docx", "analysis-v2.xlsx", "proposal.pptx", "poster.png", "material.jpg", "material.pdf"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && name == "material.pdf" {
				t.Skip("fixed POSIX PDF executable contract")
			}
			data, err := os.ReadFile(filepath.Join(output, name))
			if err != nil {
				t.Fatal(err)
			}
			if err = host.validateFile(context.Background(), name, data); err != nil {
				t.Fatalf("real library output %s rejected: %v", name, err)
			}
		})
	}
}
