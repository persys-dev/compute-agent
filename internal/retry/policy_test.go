package retry

import "testing"

func TestClassifyError_UsesKeywordMatching(t *testing.T) {
	reason := ClassifyError(assertErr{"network timeout connecting to host"})
	if reason != FailureReasonNetworkTimeout {
		t.Fatalf("expected %s, got %s", FailureReasonNetworkTimeout, reason)
	}
}

type assertErr struct{ msg string }

func (e assertErr) Error() string { return e.msg }
