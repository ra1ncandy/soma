package compiler

// ModelProfile configures paging thresholds, epoch ceilings, and cache flavor per model tier.
type ModelProfile struct {
	Name                 string
	Tier                 string // "worker" | "frontier"
	InlineThresholdBytes int
	EpochTokenCeiling    int
	MaxStalls            int
	ProviderFlavor       string // "anthropic" | "openai" | "local"
}

// WorkerProfile returns the profile tuned for 14B-70B local or low-cost worker models.
func WorkerProfile() ModelProfile {
	return ModelProfile{
		Name:                 "worker-default",
		Tier:                 "worker",
		InlineThresholdBytes: 3 * 1024,
		EpochTokenCeiling:    20000,
		MaxStalls:            2,
		ProviderFlavor:       "openai",
	}
}

// FrontierProfile returns the profile tuned for frontier models with high output-to-cache ratios.
func FrontierProfile() ModelProfile {
	return ModelProfile{
		Name:                 "frontier-default",
		Tier:                 "frontier",
		InlineThresholdBytes: 16 * 1024,
		EpochTokenCeiling:    100000,
		MaxStalls:            4,
		ProviderFlavor:       "anthropic",
	}
}

// EstimateTokens provides a fast deterministic byte-to-token approximation (~4 chars/token).
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}
