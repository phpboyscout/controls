package controls

import (
	"context"
	"fmt"
	"time"
)

// Health reports every attached child's state.
//
// This is data, not health. What a consumer does about a failed child is its
// judgement — see [Supervisor.Failures] and [WithOnFailure].
func (s *Supervisor) Health() map[string]ChildStatus {
	s.mu.Lock()
	kids := make(map[string]*supervisedChild, len(s.children))

	for n, c := range s.children {
		kids[n] = c
	}
	s.mu.Unlock()

	out := make(map[string]ChildStatus, len(kids))

	for n, c := range kids {
		out[n] = c.status()
	}

	return out
}

// Readiness reports whether the SUPERVISOR is working. It never fails because a
// child has failed.
//
// This is the point of the register/attach boundary and it is load-bearing. A
// Supervisor registered with a Controller is a [Service], and a registered
// service whose probe fails sets OverallHealthy to false for the whole process.
// If a failed child could fail this probe, one dead child would take the process
// out of rotation — exactly the coupling attaching rather than registering
// exists to avoid, arriving through the registration in the back door.
//
// A failed child is reported by [Supervisor.HealthCheck] as DEGRADED, which is
// visible to an operator and inert to a probe, and delivered to the consumer as
// a [Failure] to judge.
//
// What it does fail on is the supervisor's own lifecycle:
// [ErrSupervisorNotStarted] before Start and [ErrSupervisorStopped] once
// shutdown has begun. A stopped supervisor reporting ready is the same boundary
// failing in the other direction.
func (s *Supervisor) Readiness() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.state {
	case supNew:
		return ErrSupervisorNotStarted
	case supStopping, supStopped:
		return ErrSupervisorStopped
	case supRunning:
	}

	return nil
}

// HealthCheck returns a check reporting DEGRADED while any child has terminally
// failed, and healthy otherwise.
//
// Register it with a Controller alongside the supervisor itself. It never
// returns [CheckUnhealthy]: a supervisor with failed children is working, and
// whether an empty working set is a crisis belongs to whoever attached them.
func (s *Supervisor) HealthCheck(name string) HealthCheck {
	return HealthCheck{
		Name: name,
		Type: CheckTypeReadiness,
		Check: func(context.Context) CheckResult {
			failed, total := s.failedCount()

			if failed == 0 {
				return CheckResult{Status: CheckHealthy, Timestamp: time.Now()}
			}

			return CheckResult{
				Status:    CheckDegraded,
				Message:   fmt.Sprintf("%d of %d supervised children have failed", failed, total),
				Timestamp: time.Now(),
			}
		},
	}
}

func (s *Supervisor) failedCount() (failed, total int) {
	for _, st := range s.Health() {
		total++

		if st.State == ChildFailed {
			failed++
		}
	}

	return failed, total
}
