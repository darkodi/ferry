// Package payout owns the payout state machine and the service that moves
// payouts through it, posting the matching ledger entries.
package payout

import "slices"

// State is a payout's status. See "Payout state machine" in docs/design.md.
type State string

// Payout states. The strings match the payouts.status CHECK constraint.
const (
	Created   State = "created"
	Quoted    State = "quoted"
	Held      State = "held"
	Submitted State = "submitted"
	Settled   State = "settled"
	Reversed  State = "reversed"
	Failed    State = "failed"
	Expired   State = "expired"
)

// transitions lists every allowed move. Anything missing is forbidden.
// settled is not terminal: a bank can return the payout days later.
var transitions = map[State][]State{
	Created:   {Quoted, Failed},    // failed: the rail refused to quote
	Quoted:    {Held, Expired},     // expired: quote lapsed before confirm
	Held:      {Submitted, Failed}, // failed: the rail rejected the submit
	Submitted: {Settled, Failed},   // failed: compliance rejected after review
	Settled:   {Reversed},          // bank return
}

// CanTransition reports whether a payout may move from one state to another.
func CanTransition(from, to State) bool {
	return slices.Contains(transitions[from], to)
}

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	return len(transitions[s]) == 0
}
