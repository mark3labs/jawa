package orchestrator

import (
	"encoding/json"
	"fmt"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

// A pending resume shows "Resuming…" only until its exact continuation is
// observed executing; afterwards the normal working state is displayed while
// the resume row keeps blocking competing deliveries.
func TestResumeShowsWorkingOnceContinuationRuns(t *testing.T) {
	for _, planning := range []bool{false, true} {
		mode := "build"
		if planning {
			mode = "planning"
		}
		t.Run(mode, func(t *testing.T) {
			w, _, card, req, previous := reconcileResumeFixture(t, planning, false)
			n := 0
			send := func(target client.Target, seq int, state runtime.RunState) {
				t.Helper()
				n++
				ev := client.StatusEvent{Version: 1, Target: target, EventID: fmt.Sprintf("ev-%d", n), Type: "run_state", Seq: seq, State: state}
				if err := w.event(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
			}
			display := func() string {
				t.Helper()
				if planning {
					return planningDisplayState(planningDetails(t, w, card.ID))
				}
				a, err := w.CardResult(card.ID)
				if err != nil {
					t.Fatal(err)
				}
				return runTone(a)
			}
			if got := display(); got != "resuming" {
				t.Fatalf("display after reservation = %q, want resuming", got)
			}
			other := req.Target
			other.RunID = "other-run"
			// Replays, foreign runs, and non-running states keep the notice.
			send(req.Target, req.Seq, runtime.RunRunning)
			send(other, req.Seq+5, runtime.RunRunning)
			send(req.Target, req.Seq+5, runtime.RunWaiting)
			if notice := reconcileResumeState(t, w, req, planning, "pending", "running", ""); notice != resumePendingNotice {
				t.Fatalf("notice before continuation = %q", notice)
			}
			send(req.Target, req.Seq+6, runtime.RunRunning)
			if notice := reconcileResumeState(t, w, req, planning, "pending", "running", ""); notice != "" {
				t.Fatalf("notice after continuation = %q, want cleared", notice)
			}
			if got := display(); got != "running" {
				t.Fatalf("display after continuation = %q, want running", got)
			}
			// The resume still owns delivery: a later failure event is not applied.
			send(req.Target, req.Seq+7, runtime.RunFailed)
			reconcileResumeState(t, w, req, planning, "pending", "running", "")
			// A non-completed receipt still restores the previous failure.
			reply := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "ambiguous", Error: "lost"}
			if err := w.finishFailedResume(req.ID, reply); err != nil {
				t.Fatal(err)
			}
			reconcileResumeState(t, w, req, planning, "ambiguous", "failed", previous)
		})
	}
}

// Before the stale threshold, the dispatch loop confirms an active owner and
// switches the display without waiting for status events.
func TestResumeDispatchPromotesActiveOwner(t *testing.T) {
	for _, planning := range []bool{false, true} {
		mode := "build"
		if planning {
			mode = "planning"
		}
		t.Run(mode, func(t *testing.T) {
			w, nc, _, req, _ := reconcileResumeFixture(t, planning, false)
			w.resumeStaleAfter = 1 << 62 // never stale in this test
			queries := 0
			sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
				queries++
				raw, _ := json.Marshal(client.Status{Version: 1, Target: req.Target, State: runtime.RunRunning, Active: true, TurnID: req.TurnID, Seq: req.Seq + 3})
				_ = m.Respond(raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			resume, err := nc.Subscribe(reportprotocol.ResumeSubject("bonnie", req.Target.AgentID), func(m *nats.Msg) {
				raw, _ := json.Marshal(reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "pending"})
				_ = m.Respond(raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resume.Unsubscribe() })
			if err = nc.Flush(); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				w.dispatchFailedResumes(nc)
			}
			if notice := reconcileResumeState(t, w, req, planning, "pending", "running", ""); notice != "" {
				t.Fatalf("notice = %q, want cleared", notice)
			}
			if queries != 1 {
				t.Fatalf("owner queries = %d, want 1", queries)
			}
		})
	}
}
