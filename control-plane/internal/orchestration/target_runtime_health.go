package orchestration

type TargetRuntimeHealth struct {
	TotalPublications    int `json:"totalPublications"`
	ReadyPublications    int `json:"readyPublications"`
	FailedPublications   int `json:"failedPublications"`
	DisabledPublications int `json:"disabledPublications"`
}

type TargetRuntimeHealthProvider interface {
	HealthSnapshot() TargetRuntimeHealth
}
