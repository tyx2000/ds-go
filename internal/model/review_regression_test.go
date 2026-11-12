package model

import (
	"errors"
	"testing"
)

func TestReviewResponsesEmptyArgumentsNormalized(t *testing.T) {
	blocks, err := responseBlocks([]byte(`{"type":"function_call","call_id":"c1","name":"job_list","arguments":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_use" || string(blocks[0].Input) != "{}" {
		t.Fatalf("empty arguments were not normalized: %+v", blocks)
	}
}

func TestReviewChatTransientFinishReasonIsRetryable(t *testing.T) {
	_, err := chatStop("insufficient_system_resource")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "SERVER" || !retryable(err) {
		t.Fatalf("transient finish reason: %v", err)
	}
}

func TestReviewEmptyResponseIsNotRetried(t *testing.T) {
	if retryable(&Error{Code: "EMPTY_RESPONSE"}) {
		t.Fatal("already-settled empty response would be billed again")
	}
}
