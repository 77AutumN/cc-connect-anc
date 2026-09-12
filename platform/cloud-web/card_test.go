package cloudweb

import (
	"encoding/json"
	"github.com/chenhg5/cc-connect/core"
	"testing"
)

func TestPlainTextReviewSerializationAndSize(t *testing.T) {
	const content = "中文\n<at id=all> [批准](fake) **原文** ```🧪"
	card := core.NewCard().PlainText(content).Build()
	element := serializeCard(card)["elements"].([]map[string]any)[0]
	if element["type"] != "plain_text" || element["content"] != content {
		t.Fatal("literal source lost")
	}
	body, err := json.Marshal(serializeCard(card))
	if err != nil {
		t.Fatal(err)
	}
	card.MaxBytes = len(body)
	p := &Platform{}
	if err := p.ValidateCard(card, ""); err != nil {
		t.Fatal(err)
	}
	card.MaxBytes--
	if err := p.ValidateCard(card, ""); err != core.ErrCardTooLarge {
		t.Fatal("serialized overhead ignored")
	}
}
