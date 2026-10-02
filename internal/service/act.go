package service

import (
	"context"

	"github.com/abradner/hoist/internal/engine"
)

// ActEvent is one step's Act about to run. State is a copy of the promotion's state at that
// instant — what the Act is about to work on, not what it will produce: the outcome is the
// History entry the Act's save records, and reaches a caller through Hooks.OnHistory.
type ActEvent struct {
	Step  engine.StepName
	State engine.PromotionState
}

// announcedStep reports its own Act through onAct before it runs. Observe and Name are the
// wrapped step's.
type announcedStep struct {
	engine.Step
	onAct func(ActEvent)
}

func (a announcedStep) Act(ctx context.Context, s *engine.PromotionState) error {
	a.onAct(ActEvent{Step: a.Name(), State: *s})
	return a.Step.Act(ctx, s)
}

// announce wraps every step so a caller hears that an Act is starting. Nothing else says so:
// History records an Act once it has returned and been saved, which for a signed commit or a
// push over a slow link is exactly the stretch a caller needs a line for. A nil onAct returns
// steps unchanged, which is every caller but the CLI — the TUI draws a promotion from its
// History.
func announce(steps []engine.Step, onAct func(ActEvent)) []engine.Step {
	if onAct == nil {
		return steps
	}
	out := make([]engine.Step, len(steps))
	for i, st := range steps {
		out[i] = announcedStep{Step: st, onAct: onAct}
	}
	return out
}
