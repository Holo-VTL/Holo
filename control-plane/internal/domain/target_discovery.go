package domain

import "strings"

type TargetDiscoveryRequest struct {
	Initiator string `json:"initiator"`
	Actor     string `json:"actor,omitempty"`
	Portal    string `json:"portal,omitempty"`
}

func (r TargetDiscoveryRequest) Validate() error {
	initiator := strings.TrimSpace(r.Initiator)
	if initiator == "" {
		return ErrInvalidInput
	}
	if strings.Contains(r.Portal, " ") {
		return ErrInvalidInput
	}
	return nil
}

type DiscoverableTarget struct {
	PublicationID string `json:"publicationId"`
	TargetIQN     string `json:"targetIqn"`
	Portal        string `json:"portal"`
	State         string `json:"state"`
}
