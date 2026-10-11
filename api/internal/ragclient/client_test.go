package ragclient

import (
	"strings"
	"testing"
)

func TestParseSSE(t *testing.T) {
	body := ": keep-alive\n\nevent: token\ndata: {\"t\":\"Hel\"}\n\nevent: token\ndata: {\"t\":\"lo\"}\n\n" +
		"event: done\ndata: {\"input_tokens\":10,\n" + "data: \"output_tokens\":2}\n\n" + "data: tail"
	var got []Event
	err := ParseSSE(strings.NewReader(body), func(e Event) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 events, got %d: %+v", len(got), got)
	}
	if got[0].Name != "token" || string(got[0].Data) != `{"t":"Hel"}` {
		t.Errorf("bad first event %+v", got[0])
	}
	if got[2].Name != "done" || string(got[2].Data) != "{\"input_tokens\":10,\n\"output_tokens\":2}" {
		t.Errorf("multi-line data not joined: %q", got[2].Data)
	}
	if got[3].Name != "message" || string(got[3].Data) != "tail" {
		t.Errorf("unterminated final event not flushed: %+v", got[3])
	}
}
