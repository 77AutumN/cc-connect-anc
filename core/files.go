package core

import "errors"

const DefaultFileInputLimit = 20 << 20

// FileInputError carries a safe, localized category, never a path or input.
type FileInputError struct{ Key MsgKey }

func (e *FileInputError) Error() string { return NewI18n(LangEnglish).T(e.Key) }

func NewFileInputError(key MsgKey) error { return &FileInputError{Key: key} }

// CheckFileBatch preserves transport failures through buffering and quoting.
// Unmarked attachments retain the existing platform behavior.
func CheckFileBatch(files []FileAttachment) error {
	for _, file := range files {
		if file.ReceiveError != "" {
			return NewFileInputError(file.ReceiveError)
		}
		if file.RequireSave && len(file.Data) > DefaultFileInputLimit {
			return NewFileInputError(MsgFileInputTooLarge)
		}
	}
	return nil
}

// SaveFilesToDiskChecked reuses the existing no-overwrite writer, but never
// supplies partial input to a model. Successfully saved originals are retained.
func SaveFilesToDiskChecked(workDir, messageID string, files []FileAttachment) ([]string, error) {
	if err := CheckFileBatch(files); err != nil {
		return nil, err
	}
	paths := SaveFilesToDisk(workDir, messageID, files)
	if len(paths) != len(files) {
		return nil, NewFileInputError(MsgFileInputSaveFailed)
	}
	return paths, nil
}

func FileErrorMessage(err error, i18n *I18n) (string, bool) {
	var inputErr *FileInputError
	if errors.As(err, &inputErr) {
		return i18n.T(inputErr.Key), true
	}
	return "", false
}
