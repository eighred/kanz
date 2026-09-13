package agent

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxQuestionBytes bounds the prompt before any model or tool work. The HTTP
// body has a separate cap because JSON escaping expands the wire size.
const MaxQuestionBytes = 16 * 1024

var ErrQuestion = errors.New("copilot: question is empty, invalid UTF-8 or exceeds 16384 bytes")

func ValidateQuestion(question string) error {
	if len(question) > MaxQuestionBytes || !utf8.ValidString(question) || strings.TrimSpace(question) == "" {
		return ErrQuestion
	}
	return nil
}
