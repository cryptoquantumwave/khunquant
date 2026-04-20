package utils_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

func TestValidateSkillIdentifier_Valid(t *testing.T) {
	valid := []string{
		"my-skill",
		"skill123",
		"SKILL",
		"a",
		"skill_name",
	}
	for _, id := range valid {
		if err := utils.ValidateSkillIdentifier(id); err != nil {
			t.Errorf("identifier %q: unexpected error: %v", id, err)
		}
	}
}

func TestValidateSkillIdentifier_Empty(t *testing.T) {
	if err := utils.ValidateSkillIdentifier(""); err == nil {
		t.Error("expected error for empty identifier")
	}
}

func TestValidateSkillIdentifier_Whitespace(t *testing.T) {
	if err := utils.ValidateSkillIdentifier("   "); err == nil {
		t.Error("expected error for whitespace-only identifier")
	}
}

func TestValidateSkillIdentifier_ForwardSlash(t *testing.T) {
	if err := utils.ValidateSkillIdentifier("path/traversal"); err == nil {
		t.Error("expected error for identifier containing '/'")
	}
}

func TestValidateSkillIdentifier_Backslash(t *testing.T) {
	if err := utils.ValidateSkillIdentifier("path\\traversal"); err == nil {
		t.Error("expected error for identifier containing '\\'")
	}
}

func TestValidateSkillIdentifier_DotDot(t *testing.T) {
	if err := utils.ValidateSkillIdentifier("../etc"); err == nil {
		t.Error("expected error for identifier containing '..'")
	}
}

func TestValidateSkillIdentifier_OnlyDotDot(t *testing.T) {
	if err := utils.ValidateSkillIdentifier(".."); err == nil {
		t.Error("expected error for '..' identifier")
	}
}
