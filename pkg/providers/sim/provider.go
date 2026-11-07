// Package sim is the simulated LLM of a sandbox instance: the provider behind the
// model ref "sim/<mode>". It exists only in a locked process and answers from rules the
// lab scripts. It imports neither pkg/providers nor pkg/tools (both sit above it): the
// gateway hands it what it needs to know about a turn (EngineOptions.TurnInfo).
package sim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// The modes of the simulated model. scripted answers from ordered match/respond rules,
// replay from a cassette of recorded answers; record and live come with the next PR.
const (
	ModeScripted = "scripted"
	ModeReplay   = "replay"
)

// attached is the engine every Provider of the process talks to. The model factory's
// signature has no way to pass one in, so it is process state, set under the lock.
var attached atomic.Pointer[Engine]

// Attach makes e the process's engine. It refuses unless the process is locked, and
// while another engine is attached.
func Attach(e *Engine) error {
	if e == nil {
		return errors.New("sim: nil engine")
	}
	if !sandbox.Locked() {
		return errors.New("sim: an engine can only be attached in a sandbox-locked process")
	}
	if !attached.CompareAndSwap(nil, e) {
		return errors.New("sim: an engine is already attached")
	}
	return nil
}

// Detach removes e if it is the attached engine (the lab's teardown calls it).
func Detach(e *Engine) { attached.CompareAndSwap(e, nil) }

// Provider is what the model factory builds for "sim/<mode>". It holds no state: every
// call goes to the attached engine, and fails closed when there is none.
type Provider struct{ mode string }

// NewProvider builds the provider for a mode. It refuses outside a locked process.
func NewProvider(mode string) (*Provider, error) {
	if !sandbox.Locked() {
		return nil, errors.New("sim: the simulated model runs only in a sandbox-locked process")
	}
	if mode != ModeScripted && mode != ModeReplay {
		return nil, fmt.Errorf("sim: mode %q is not available (only %q and %q)", mode, ModeScripted, ModeReplay)
	}
	return &Provider{mode: mode}, nil
}

// ParseRef splits what follows "sim/" in a model ref, "replay" or "live:<protocol>/<model>",
// into the mode and the inner model ("" for modes with none).
func ParseRef(id string) (mode, inner string) {
	mode, inner, _ = strings.Cut(id, ":")
	return mode, inner
}

// Ref is the model ref of a mode and its inner model.
func Ref(mode, inner string) string {
	if inner == "" {
		return "sim/" + mode
	}
	return "sim/" + mode + ":" + inner
}

// Name is the provider's protocol.
func (p *Provider) Name() string { return "sim" }

// GetDefaultModel is the model ref.
func (p *Provider) GetDefaultModel() string { return "sim/" + p.mode }

// Chat answers through the attached engine.
func (p *Provider) Chat(ctx context.Context, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition,
	model string, options map[string]any,
) (*protocoltypes.LLMResponse, error) {
	e := attached.Load()
	if e == nil {
		return nil, errors.New("sim: no engine is attached, so the simulated model cannot answer")
	}
	// The mode is the instance's, fixed at start: a model ref of another mode (a config
	// reload that switched sim/scripted to sim/replay) does not change what answers.
	if e.Mode() != p.mode {
		return nil, fmt.Errorf("sim: this instance runs sim/%s; the model ref says sim/%s", e.Mode(), p.mode)
	}
	return e.Chat(ctx, msgs, tools, model, options)
}
