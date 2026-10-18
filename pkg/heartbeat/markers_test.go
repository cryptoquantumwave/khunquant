package heartbeat

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// pkg/tools cannot import this package, and names the upgrade command in the refusal of a
// memory write by a v1-template heartbeat by recognising the template itself. The two
// copies of its markers are held equal here.
func TestToolsRecogniseTheSameTemplates(t *testing.T) {
	if tools.HeartbeatV1Marker != V1Marker {
		t.Errorf("tools.HeartbeatV1Marker = %q, heartbeat.V1Marker = %q", tools.HeartbeatV1Marker, V1Marker)
	}
	if tools.HeartbeatV2Marker != V2Marker {
		t.Errorf("tools.HeartbeatV2Marker = %q, heartbeat.V2Marker = %q", tools.HeartbeatV2Marker, V2Marker)
	}
	if !IsV2Template(TemplateV2()) || IsV1Template(TemplateV2()) {
		t.Error("the shipped v2 template is not recognised as v2 alone")
	}
}
