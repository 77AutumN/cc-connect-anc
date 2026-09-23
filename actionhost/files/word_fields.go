package files

import (
	"encoding/xml"
	"regexp"
	"strings"
)

const wordNamespace = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"

// Match whole instructions. Field names alone are insufficient: switches and
// split/nested instructions can otherwise smuggle active references through.
var pageField = regexp.MustCompile(`(?i)^\s*(?:PAGE|NUMPAGES)(?:\s+\\\*\s+(?:MERGEFORMAT|Arabic|ROMAN|ALPHABETIC))*\s*$`)
var tocField = regexp.MustCompile(`(?i)^\s*TOC(?:\s+\\(?:[hzu]|[on]\s+"[1-9]-[1-9]"|\*\s+MERGEFORMAT))*\s*$`)
var pageReference = regexp.MustCompile(`(?i)^\s*PAGEREF\s+(?:[A-Z_][A-Z0-9_.-]{0,79}|"[A-Z_][A-Z0-9_.-]{0,79}")(?:\s+\\(?:[hp]|\*\s+MERGEFORMAT))*\s*$`)

func staticWordField(value string) bool {
	return len(value) <= 1024 && (pageField.MatchString(value) || tocField.MatchString(value) || pageReference.MatchString(value))
}

type wordField struct {
	code   strings.Builder
	result bool
}

type wordFields struct {
	stack       []wordField
	instruction bool
}

func (f *wordFields) start(t xml.StartElement, allowed bool) error {
	if t.Name.Local != "fldSimple" && t.Name.Local != "fldChar" && t.Name.Local != "instrText" {
		return nil
	}
	if !allowed || t.Name.Space != wordNamespace || f.instruction {
		return ErrInvalid
	}
	attr := func(name string) string {
		for _, a := range t.Attr {
			if a.Name.Space == wordNamespace && a.Name.Local == name {
				return a.Value
			}
		}
		return ""
	}
	switch t.Name.Local {
	case "fldSimple":
		if !staticWordField(attr("instr")) || (len(f.stack) > 0 && !f.stack[len(f.stack)-1].result) {
			return ErrInvalid
		}
	case "instrText":
		if len(f.stack) == 0 || f.stack[len(f.stack)-1].result {
			return ErrInvalid
		}
		f.instruction = true
	case "fldChar":
		switch attr("fldCharType") {
		case "begin":
			if len(f.stack) >= 32 || (len(f.stack) > 0 && !f.stack[len(f.stack)-1].result) {
				return ErrInvalid
			}
			f.stack = append(f.stack, wordField{})
		case "separate", "end":
			if len(f.stack) == 0 {
				return ErrInvalid
			}
			last := &f.stack[len(f.stack)-1]
			if !staticWordField(last.code.String()) {
				return ErrInvalid
			}
			if attr("fldCharType") == "separate" {
				if last.result {
					return ErrInvalid
				}
				last.result = true
			} else {
				f.stack = f.stack[:len(f.stack)-1]
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}

func (f *wordFields) text(data []byte) error {
	if f.instruction {
		last := &f.stack[len(f.stack)-1]
		if last.code.Len()+len(data) > 1024 {
			return ErrInvalid
		}
		last.code.Write(data)
	}
	return nil
}
