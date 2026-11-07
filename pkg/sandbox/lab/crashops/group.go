// Package crashops is the lab group "crash": arm, disarm and list crash points. It is a
// package of its own because crash imports the journal and lab holds the arms' owner, so
// crash must not import lab.
package crashops

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

const description = "Make the instance die the way kill -9 kills it, at a named step of a workflow. " +
	"arm {workflow (kind, e.g. dn.open), step (e.g. place_second_leg), point (before_started|after_started|after_fn|after_output), attempt?} " +
	"answers {id}; the next time that point is reached the process is killed (SIGKILL) and the supervisor restarts it on the same home. " +
	"An arm fires once and is not kept across the restart. A step that never runs is never reported as missing: list shows what is still armed. " +
	"fired lists the crashes earlier processes died of, from the journal."

// Group lets an agent arm and inspect crash points.
func Group(a *crash.Arms) lab.Group {
	return lab.Group{
		Name:        "crash",
		Description: description,
		Ops: []lab.Op{
			{
				Name:        "arm",
				Description: "arm one crash point; answers its id",
				Args: json.RawMessage(`{"workflow":{"type":"string","description":"workflow kind, e.g. dn.open"},"step":{"type":"string"},` +
					`"point":{"type":"string","enum":["before_started","after_started","after_fn","after_output"]},"attempt":{"type":"integer","description":"only this attempt of the step; default any"}}`),
				Required: []string{"workflow", "step", "point"},
				Run:      armOp(a),
			},
			{
				Name: "disarm", Description: "remove an arm; answers {removed}",
				Args: json.RawMessage(`{"id":{"type":"string"}}`), Required: []string{"id"}, Run: disarmOp(a),
			},
			{Name: "list", Description: "{armed: the points still armed, fired: the crashes the journal holds}", Run: listOp(a)},
		},
	}
}

func decode(args json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	return nil
}

func armOp(a *crash.Arms) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct {
			Op string `json:"op"`
			crash.Arm
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		id, err := a.Arm(in.Arm)
		if err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		return map[string]any{"id": id}, nil
	}
}

func disarmOp(a *crash.Arms) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var in struct{ Op, ID string }
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return map[string]any{"removed": a.Disarm(in.ID)}, nil
	}
}

func listOp(a *crash.Arms) lab.OpFunc {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		armed, fired, err := a.List(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"armed": armed, "fired": fired}, nil
	}
}
