package httpapi

import (
	"context"
	"net/http"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

func (a *API) submitTask(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	cmd, err := command(r, actor, false)
	if err != nil {
		return err
	}
	body, err := decodeLimited[SubmitTaskRequest](r, maxSubmitTaskBodyBytes)
	if err != nil {
		return err
	}
	submission, err := application.Submit(r.Context(), a.pool, application.SubmitTask{
		Command: cmd, At: a.options.Now(), Provider: body.Provider, Demand: body.Demand, Deadline: body.Deadline,
		Brief: lifecycle.ExecutionBrief{Objective: body.Objective, AcceptanceCriteria: body.AcceptanceCriteria},
		Unit:  body.Unit, Ceiling: body.Ceiling, Amount: body.Amount, Spend: body.Spend,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tenants/"+actor.Tenant().String()+"/tasks/"+submission.Task.ID.String())
	w.Header().Set("ETag", etag(submission.Task.Version))
	writeJSON(w, http.StatusCreated, submissionBody(submission))
	return nil
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	view, err := application.GetTask(r.Context(), a.pool, actor, task)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(view.Task.Version))
	writeJSON(w, http.StatusOK, taskBody(view))
	return nil
}

// assertSpend completes a spend-governed submission or rework. If-Match
// carries the spend challenge's version.
func (a *API) assertSpend(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	body, err := decode[SpendAssertionRequest](r)
	if err != nil {
		return err
	}
	if body.Assertion.Terms.Task != task {
		return application.ErrNotFound
	}
	if err := a.visibleTask(r.Context(), actor, task); err != nil {
		return err
	}
	submission, err := application.Assert(r.Context(), a.pool, application.AssertSpend{
		Command: cmd, SubmissionNonce: body.SubmissionKey, ReviewNonce: body.ReviewKey, Assertion: body.Assertion,
	})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(submission.Task.Version))
	writeJSON(w, http.StatusOK, submissionBody(submission))
	return nil
}

// cancelTask requests cancellation. Cancellation is an intent until the stop
// is observed, so success is 202.
func (a *API) cancelTask(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	if _, err := decode[CancelRequest](r); err != nil {
		return err
	}
	if err := a.visibleTask(r.Context(), actor, task); err != nil {
		return err
	}
	view, err := application.Cancel(r.Context(), a.pool, application.CancelTask{Command: cmd, Task: task, At: a.options.Now()})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, CancelBody{
		Task: view.TaskID, State: view.State, Attempt: view.AttemptID, AttemptState: view.AttemptState, Reason: view.Reason,
	})
	return nil
}

// reviewResult records a review. If-Match carries the task version.
func (a *API) reviewResult(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	body, err := decode[ReviewRequest](r)
	if err != nil {
		return err
	}
	if err := a.visibleTask(r.Context(), actor, task); err != nil {
		return err
	}
	view, err := application.ReviewResult(r.Context(), a.pool, application.ReviewResultCommand{
		Command: cmd, Task: task, Revision: body.Revision, Disposition: body.Disposition, Rework: body.Rework, At: a.options.Now(),
	})
	if err != nil {
		return err
	}
	response := ReviewBody{Review: view.Review}
	if view.Rework != nil {
		rework := submissionBody(*view.Rework)
		response.Rework = &rework
	}
	writeJSON(w, http.StatusCreated, response)
	return nil
}

func (a *API) getAttempt(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	attempt, err := pathID[ids.Attempt](r, "attempt")
	if err != nil {
		return err
	}
	loaded, err := application.GetAttempt(r.Context(), a.pool, actor, task, attempt)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(loaded.Version))
	writeJSON(w, http.StatusOK, loaded)
	return nil
}

func (a *API) getRevision(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	revision, err := pathID[ids.Revision](r, "revision")
	if err != nil {
		return err
	}
	view, err := application.GetRevision(r.Context(), a.pool, actor, task, revision)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, revisionBody(view))
	return nil
}

func (a *API) listAttention(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	requests, err := application.ListAttention(r.Context(), a.pool, actor)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, AttentionListBody{Attention: nonNil(requests)})
	return nil
}

func (a *API) getAttention(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	attention, err := pathID[ids.Attention](r, "attention")
	if err != nil {
		return err
	}
	request, err := application.GetAttention(r.Context(), a.pool, actor, attention)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", etag(request.Version))
	writeJSON(w, http.StatusOK, request)
	return nil
}

// decide answers an attention request. If-Match carries the request version.
func (a *API) decide(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	attention, err := pathID[ids.Attention](r, "attention")
	if err != nil {
		return err
	}
	cmd, err := command(r, actor, true)
	if err != nil {
		return err
	}
	body, err := decode[DecisionRequest](r)
	if err != nil {
		return err
	}
	if err := a.visibleAttention(r.Context(), actor, attention); err != nil {
		return err
	}
	view, err := application.Decide(r.Context(), a.pool, application.DecideAttention{
		Command: cmd, Attention: attention, Kind: body.Kind, AnswerDigest: body.AnswerDigest, At: a.options.Now(),
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, DecisionBody{
		Attention: view.AttentionID, Decision: view.DecisionID, Kind: view.Kind, State: view.State, Grant: view.GrantID,
	})
	return nil
}

// visibleTask refuses a command on a task the actor may not read with the
// same not-found a missing task gets, so a command never reveals a task that
// a read would hide. Commands still apply their own authorization after it.
func (a *API) visibleTask(ctx context.Context, actor identity.AuthorizationContext, task ids.TaskID) error {
	_, err := application.GetTask(ctx, a.pool, actor, task)
	return err
}

// visibleAttention is visibleTask for an attention request.
func (a *API) visibleAttention(ctx context.Context, actor identity.AuthorizationContext, attention ids.AttentionID) error {
	_, err := application.GetAttention(ctx, a.pool, actor, attention)
	return err
}
