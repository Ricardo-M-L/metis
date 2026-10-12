package desktopipc

import (
	"strings"
	"testing"
)

func TestCheckpointRequiresExplicitDurableAcknowledgement(t *testing.T) {
	for _, frame := range []string{
		`{"version":1,"type":"checkpoint","checkpointed":true}`,
		`{"version":1,"type":"checkpoint"}`,
		`{"version":1,"type":"checkpoint","checkpointed":false}`,
		`{"version":1,"type":"checkpoint","checkpointed":true,"input":"new input"}`,
	} {
		message, err := NewDecoder(strings.NewReader(frame + "\n")).Decode()
		wantValid := frame == `{"version":1,"type":"checkpoint","checkpointed":true}`
		if (err == nil) != wantValid {
			t.Fatalf("checkpoint validation %s: %+v %v", frame, message, err)
		}
	}
}
