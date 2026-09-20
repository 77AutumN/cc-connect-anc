package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveFilesToDiskCheckedRejectsIncompleteInput(t *testing.T) {
	for _, mode := range []string{"receive", "storage", "collision"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			files := []FileAttachment{{FileName: "fictional.txt", Data: []byte("fictional"), RequireSave: true}}
			want := MsgFileInputSaveFailed
			switch mode {
			case "receive":
				files[0].ReceiveError = MsgFileInputUnavailable
				want = MsgFileInputUnavailable
			case "storage":
				if err := os.WriteFile(filepath.Join(dir, ".cc-connect"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "collision":
				if _, err := SaveFilesToDiskChecked(dir, "", files); err != nil {
					t.Fatal(err)
				}
				files = append([]FileAttachment{{FileName: "second.txt", Data: []byte("new"), RequireSave: true}}, files...)
			}
			paths, err := SaveFilesToDiskChecked(dir, "", files)
			var inputErr *FileInputError
			if paths != nil || !errors.As(err, &inputErr) || inputErr.Key != want {
				t.Fatalf("partial input escaped: paths=%d err=%v", len(paths), err)
			}
		})
	}
}

func TestCheckedFileInputRetainsOriginalsAndLegacyDefault(t *testing.T) {
	dir := t.TempDir()
	files := []FileAttachment{{FileName: "sample.txt", Data: []byte("first"), RequireSave: true}}
	first, err := SaveFilesToDiskChecked(dir, "fictional-message", files)
	if err != nil {
		t.Fatal(err)
	}
	files[0].Data = []byte("second")
	second, err := SaveFilesToDiskChecked(dir, "fictional-message", files)
	if err != nil || first[0] == second[0] {
		t.Fatalf("original replaced: %v", err)
	}
	data, err := os.ReadFile(first[0])
	if err != nil || string(data) != "first" {
		t.Fatal("original input lost")
	}
	large := FileAttachment{Data: make([]byte, DefaultFileInputLimit+1)}
	if err := CheckFileBatch([]FileAttachment{large}); err != nil {
		t.Fatal("legacy input behavior changed")
	}
	large.RequireSave = true
	if err := CheckFileBatch([]FileAttachment{large}); err == nil {
		t.Fatal("controlled input exceeded budget")
	}
}
