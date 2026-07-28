package claude

func NormalizeCacheCreationSplit(totalTokens int, tokens5m int, tokens1h int) (int, int) {
	if totalTokens < 0 {
		totalTokens = 0
	}
	if tokens5m < 0 {
		tokens5m = 0
	}
	if tokens1h < 0 {
		tokens1h = 0
	}
	// Reduce the total in ordered, guarded steps. Direct subtraction of all
	// three untrusted values can overflow before the remainder is clamped.
	remainder := 0
	if totalTokens > tokens5m {
		remainder = totalTokens - tokens5m
		if remainder > tokens1h {
			remainder -= tokens1h
		} else {
			remainder = 0
		}
	}
	return tokens5m + remainder, tokens1h
}
