package files

import (
	"os"
	"os/exec"
	"path/filepath"
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
for line in Path(sys.argv[1]).read_text().splitlines():
    if line and not line.startswith('#'):
        name,version=line.split('==')
        assert m.version(name)==version, name
root=Path(sys.argv[2])
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
	for _, name := range []string{"analysis-v1.docx", "analysis-v1.xlsx", "analysis-v2.docx", "analysis-v2.xlsx"} {
		data, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = validateOOXML(name, data); err != nil {
			t.Fatalf("real library output %s rejected: %v", name, err)
		}
	}
}
