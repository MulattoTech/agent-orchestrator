package inferenceguard

import (
	"context"
)

type DispatchAudit struct {
	EffectiveConfirmation string `json:"effectiveConfirmation"`
	ID                    string `json:"id"`
	Session               string `json:"session"`
	Project               string `json:"project"`
	Role                  string `json:"role"`
	Operation             string `json:"operation"`
	RequestedModel        string `json:"requestedModel"`
	RequestedEffort       string `json:"requestedEffort"`
	ResolvedModel         string `json:"resolvedModel"`
	ResolvedEffort        string `json:"resolvedEffort"`
	EffectiveModel        string `json:"effectiveModel"`
	EffectiveEffort       string `json:"effectiveEffort"`
	State                 string `json:"state"`
	ExceptionID           string `json:"exceptionId"`
	ErrorClass            string `json:"errorClass"`
	DispatchedAt          string `json:"dispatchedAt"`
}

func (g *Gate) RecentDispatches(ctx context.Context) ([]DispatchAudit, error) {
	rows, err := g.db.QueryContext(ctx, "SELECT id,session_id,project_id,role,operation,requested_model,requested_effort,model,effort,state,exception_id,error_class,dispatched_at FROM attempts ORDER BY created_at DESC,id DESC LIMIT 40")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DispatchAudit{}
	for rows.Next() {
		var a DispatchAudit
		if err = rows.Scan(&a.ID, &a.Session, &a.Project, &a.Role, &a.Operation, &a.RequestedModel, &a.RequestedEffort, &a.ResolvedModel, &a.ResolvedEffort, &a.State, &a.ExceptionID, &a.ErrorClass, &a.DispatchedAt); err != nil {
			return nil, err
		}
		if a.DispatchedAt != "" {
			a.EffectiveModel = a.ResolvedModel
			a.EffectiveEffort = a.ResolvedEffort
		}
		a.EffectiveConfirmation = "serialized outbound selection; server and native internal requests unverified"
		out = append(out, a)
	}
	return out, rows.Err()
}
