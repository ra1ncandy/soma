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

// WorkerProfile returns the profile tuned for fast/low-cost worker models.
func WorkerProfile() ModelProfile {
	return ModelProfile{
		Name:                 "worker-default",
		Tier:                 "worker",
		InlineThresholdBytes: 8 * 1024,
		EpochTokenCeiling:    48000,
		MaxStalls:            3,
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

// ResearchProfile returns the profile tuned for long-horizon scientific, AI, biology, and security research.
func ResearchProfile() ModelProfile {
	return ModelProfile{
		Name:                 "research-deep",
		Tier:                 "frontier",
		InlineThresholdBytes: 16 * 1024,
		EpochTokenCeiling:    120000,
		MaxStalls:            4,
		ProviderFlavor:       "openai",
	}
}

// EstimateTokens provides a fast deterministic byte-to-token approximation (~4 chars/token).
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}
