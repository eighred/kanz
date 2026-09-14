package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
)

func TestQuestionBoundAppliesBeforeModelConstruction(t *testing.T) {
	a := &Agent{}
	for _, question := range []string{"", " \n", strings.Repeat("x", MaxQuestionBytes+1), string([]byte{0xff})} {
		_, err := a.Ask(context.Background(), &auth.Principal{Subject: "test-actor", Tenant: "test-tenant"}, question)
		if !errors.Is(err, ErrQuestion) {
			t.Fatalf("unsupported question accepted: %v", err)
		}
	}
	if err := ValidateQuestion(strings.Repeat("x", MaxQuestionBytes)); err != nil {
		t.Fatal(err)
	}
}
