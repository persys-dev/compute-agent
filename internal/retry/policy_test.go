package retry

import (
	"errors"
	"testing"
)

func TestContains_MatchesKeyword(t *testing.T) {
	if !contains("failed to pull image manifest", "manifest") {
		t.Fatalf("expected contains to match keyword")
	}
}

func TestContains_CaseInsensitive(t *testing.T) {
	if !contains("Connection Refused", "connection refused") {
		t.Fatalf("expected contains to be case-insensitive")
	}
}

func TestClassifyError_ImagePullTimeout(t *testing.T) {
	err := errors.New("Image pull timed out while fetching manifest")
	got := ClassifyError(err)
	if got != FailureReasonImagePullTimeout {
		t.Fatalf("expected %s, got %s", FailureReasonImagePullTimeout, got)
	}
}
