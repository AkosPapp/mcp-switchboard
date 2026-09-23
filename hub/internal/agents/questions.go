package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/push"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// switchboard.user.ask: the model asks the user questions and waits.
//
// It reuses the approval machinery, because the shape is the same: the run
// leaves its slot and shows as blocked, a frame and a push notification tell
// the user, and a decision (here, the answers) resumes it. A pending question
// is a pendingApproval whose `questions` field is set; it is listed wherever
// approvals are (GET /api/approvals, GET /api/runs/{id}) and answered with
// POST /api/runs/{id}/questions/{callId}.

const (
	maxQuestions = 4
	maxOptions   = 6
)

// Question is one question of a switchboard.user.ask call.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}

// QuestionOption is one suggested answer.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionAnswer answers the question at the same position: the chosen option
// labels and/or the user's own words.
type QuestionAnswer struct {
	Answers []string `json:"answers"`
}

func (pa *pendingApproval) view(callID string) PendingApproval {
	return PendingApproval{CallID: callID, Tool: pa.tool, Arguments: pa.arguments,
		ExpiresAt: store.FormatTime(pa.expiresAt), Questions: pa.questions}
}

// parseQuestions reads the tool's `questions` argument, which arrives as
// generic JSON, and rejects what the user could not sensibly be shown.
func parseQuestions(args map[string]any) ([]Question, error) {
	raw, err := json.Marshal(args["questions"])
	if err != nil {
		return nil, err
	}
	var in []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multi_select"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in) == 0 {
		return nil, errors.New("questions must be a non-empty array of {question, options?}")
	}
	if len(in) > maxQuestions {
		return nil, fmt.Errorf("ask at most %d questions at a time", maxQuestions)
	}
	out := make([]Question, 0, len(in))
	for i, q := range in {
		text := strings.TrimSpace(q.Question)
		if text == "" {
			return nil, fmt.Errorf("question %d has no text", i+1)
		}
		if len(q.Options) > maxOptions {
			return nil, fmt.Errorf("question %d has more than %d options", i+1, maxOptions)
		}
		nq := Question{Question: text, Header: strings.TrimSpace(q.Header), MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				return nil, fmt.Errorf("question %d has an option without a label", i+1)
			}
			nq.Options = append(nq.Options, QuestionOption{Label: label, Description: strings.TrimSpace(o.Description)})
		}
		out = append(out, nq)
	}
	return out, nil
}

func (m *Manager) toolAsk(ctx context.Context, cc *callCtx, args map[string]any) (any, error) {
	rs := cc.rs
	if rs == nil {
		return nil, errors.New("there is no run to pause: user.ask only works inside a chat run, not through /mcp/agent")
	}
	qs, err := parseQuestions(args)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(m.set.ApprovalTimeout) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	now := time.Now().UTC()
	pa := &pendingApproval{
		tool: "switchboard.user.ask", arguments: args, createdAt: now, expiresAt: now.Add(timeout),
		ch: make(chan approvalDecision, 1), questions: qs,
	}
	m.mu.Lock()
	rs.approvals[cc.callID] = pa
	m.mu.Unlock()

	m.enterWait(rs, true)
	m.frame(rs, "approval_required", map[string]any{
		"callId": cc.callID, "name": pa.tool, "arguments": args, "questions": qs, "expiresAt": store.FormatTime(pa.expiresAt),
	})
	m.publish(eventChat(rs))
	m.publish(events.Event{Type: events.TypeAgent, AgentID: rs.agentID}) // /api/approvals changed
	m.notifyPush(rs, push.KindQuestion, questionNotice(qs))

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var dec approvalDecision
	var stop error
	select {
	case dec = <-pa.ch:
	case <-timer.C:
		stop = fmt.Errorf("the user did not answer within %s", timeout.Round(time.Second))
	case <-ctx.Done():
		stop = errors.New("cancelled while waiting for the user")
	}
	m.mu.Lock()
	delete(rs.approvals, cc.callID)
	m.mu.Unlock()
	m.publish(events.Event{Type: events.TypeAgent, AgentID: rs.agentID})
	m.leaveWait(rs, true)

	if stop != nil {
		return nil, stop
	}
	if !dec.approved {
		return nil, errors.New("the user declined to answer; carry on with your best judgement or explain what you need")
	}
	type answered struct {
		Question string   `json:"question"`
		Answers  []string `json:"answers"`
	}
	out := make([]answered, len(qs))
	for i, q := range qs {
		out[i] = answered{Question: q.Question, Answers: dec.answers[i].Answers}
	}
	return map[string]any{"answers": out}, nil
}

// questionNotice is what the push notification says.
func questionNotice(qs []Question) string {
	first := qs[0].Question
	if r := []rune(first); len(r) > 120 {
		first = string(r[:120]) + "…"
	}
	if len(qs) > 1 {
		return fmt.Sprintf("asks: %s (+%d more)", first, len(qs)-1)
	}
	return "asks: " + first
}

// Answer implements Service.
func (m *Manager) Answer(_ context.Context, runID, callID string, answers []QuestionAnswer) error {
	m.mu.Lock()
	rs := m.runs[runID]
	var pa *pendingApproval
	if rs != nil {
		pa = rs.approvals[callID]
	}
	m.mu.Unlock()
	if pa == nil || pa.questions == nil {
		return ErrNotPending
	}
	if len(answers) != len(pa.questions) {
		return fmt.Errorf("%w: expected %d answers, got %d", ErrInvalid, len(pa.questions), len(answers))
	}
	for i, a := range answers {
		kept := a.Answers[:0:0]
		for _, s := range a.Answers {
			if s = strings.TrimSpace(s); s != "" {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("%w: question %d has no answer", ErrInvalid, i+1)
		}
		answers[i].Answers = kept
	}
	select {
	case pa.ch <- approvalDecision{approved: true, answers: answers}:
		return nil
	default:
		return ErrNotPending // already answered
	}
}
