package payout

import "testing"

func TestTransitions(t *testing.T) {
	all := []State{Created, Quoted, Held, Submitted, Settled, Reversed, Failed, Expired}
	allowed := map[[2]State]bool{
		{Created, Quoted}: true, {Created, Failed}: true,
		{Quoted, Held}: true, {Quoted, Expired}: true,
		{Held, Submitted}: true, {Held, Failed}: true,
		{Submitted, Settled}: true, {Submitted, Failed}: true,
		{Settled, Reversed}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := CanTransition(from, to), allowed[[2]State{from, to}]; got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range all {
		want := s == Reversed || s == Failed || s == Expired
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), want)
		}
	}
}
