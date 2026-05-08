package orchestration

type TargetDiscoveryHealth struct {
	TotalQueries    int `json:"totalQueries"`
	LastVisible     int `json:"lastVisible"`
	DiscoverableNow int `json:"discoverableNow"`
}

type TargetDiscoveryHealthProvider interface {
	DiscoverySnapshot() TargetDiscoveryHealth
}
