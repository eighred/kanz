// Package numerics contains numerical primitives shared by risk measures.
package numerics

import "math"

// Sum retains small signed terms across offsetting large terms using
// Neumaier compensated summation. Its zero value is ready to use.
type Sum struct {
	total, correction float64
}

func (s *Sum) Add(term float64) {
	next := s.total + term
	if math.Abs(s.total) >= math.Abs(term) {
		s.correction += (s.total - next) + term
	} else {
		s.correction += (term - next) + s.total
	}
	s.total = next
}

func (s Sum) Value() float64 { return s.total + s.correction }
